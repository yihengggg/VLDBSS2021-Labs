package commands

import (
	"encoding/hex"

	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap/log"
	"go.uber.org/zap"
)

// Prewrite represents the prewrite stage of a transaction. A prewrite contains all writes (but not reads) in a transaction,
// if the whole transaction can be written to underlying storage atomically and without conflicting with other
// transactions (complete or in-progress) then success is returned to the client. If all a client's prewrites succeed,
// then it will send a commit message. I.e., prewrite is the first phase in a two phase commit.
// Prewrite表示事务的预写阶段。预写包含事务中的所有写操作（但不包括读操作），
// 如果整个事务可以原子地写入底层存储，并且不与其他事务（已完成或进行中）冲突，则返回成功给客户端。
// 如果客户端的所有预写操作都成功，则会发送提交消息。即，预写是两阶段提交的第一阶段。
type Prewrite struct {
	CommandBase
	request *kvrpcpb.PrewriteRequest
}

func NewPrewrite(request *kvrpcpb.PrewriteRequest) Prewrite {
	return Prewrite{
		CommandBase: CommandBase{
			context: request.Context,
			startTs: request.StartVersion,
		},
		request: request,
	}
}

// PrepareWrites prepares the data to be written to the raftstore. The data flow is as follows.
// The tinysql part:
//
//			user client -> insert/delete query -> tinysql server
//	     query -> parser -> planner -> executor -> the transaction memory buffer
//			memory buffer -> kv muations -> kv client 2pc committer
//			committer -> prewrite all the keys
//			committer -> commit all the keys
//			tinysql server -> respond to the user client
//
// The tinykv part:
//
//			prewrite requests -> transaction mutations -> raft request
//			raft req -> raft router -> raft worker -> peer propose raft req
//			raft worker -> peer receive majority response for the propose raft req  -> peer raft committed entries
//	 	raft worker -> process committed entries -> send apply req to apply worker
//			apply worker -> apply the correspond requests to storage(the state machine) -> callback
//			callback -> signal the response action -> response to kv client
func (p *Prewrite) PrepareWrites(txn *mvcc.MvccTxn) (interface{}, error) {
	response := new(kvrpcpb.PrewriteResponse)

	// Prewrite all mutations in the request.
	for _, m := range p.request.Mutations {
		keyError, err := p.prewriteMutation(txn, m)
		if keyError != nil {
			response.Errors = append(response.Errors, keyError)
		} else if err != nil {
			return nil, err
		}
	}

	return response, nil
}

// prewriteMutation prewrites mut to txn. It returns (nil, nil) on success, (err, nil) if the key in mut is already
// locked or there is any other key error, and (nil, err) if an internal error occurs.
// 将此事务涉及写入的所有 key 上锁并写入 value。
func (p *Prewrite) prewriteMutation(txn *mvcc.MvccTxn, mut *kvrpcpb.Mutation) (*kvrpcpb.KeyError, error) {
	key := mut.Key
	log.Debug("prewrite key", zap.Uint64("start_ts", txn.StartTS),
		zap.String("key", hex.EncodeToString(key)))
	// YOUR CODE HERE (lab2).
	// Check for write conflicts.
	// Hint: Check the interafaces provided by `mvcc.MvccTxn`. The error type `kvrpcpb.WriteConflict` is used
	//		 denote to write conflict error, try to set error information properly in the `kvrpcpb.KeyError`
	//		 response.
	// 约束性检查：找最新的一个 write 记录，比较其 commit_ts 和当前事务的 start_ts 来判断是否发生冲突。
	write, commitTs, err := txn.MostRecentWrite(key)
	if write != nil && commitTs >= txn.StartTS {
		keyError := kvrpcpb.KeyError{
			Conflict: &kvrpcpb.WriteConflict{
				StartTs:    txn.StartTS,
				ConflictTs: commitTs,
				Key:        key,
				Primary:    p.request.PrimaryLock,
			},
		}
		return &keyError, nil
	} else if err != nil {
		return nil, err
	}
	// YOUR CODE HERE (lab2).
	// Check if key is locked. Report key is locked error if lock does exist, note the key could be locked
	// by this transaction already and the current prewrite request is stale.
	// 检查key是否已经被另一个事务上锁
	keyLock, err := txn.GetLock(key)
	if keyLock != nil {
		if keyLock.Ts != txn.StartTS {
			keyError := kvrpcpb.KeyError{Locked: keyLock.Info(key),
				Conflict: &kvrpcpb.WriteConflict{
					StartTs:    txn.StartTS,
					ConflictTs: keyLock.Ts, // 锁的时间戳
					Key:        key,
					Primary:    p.request.PrimaryLock,
				},
			}
			return &keyError, nil
		} else {
			// 被同一个事务上锁，保证幂等性，即允许重复收到同一个请求
			return nil, nil
		}
	}
	// YOUR CODE HERE (lab2).
	// Write a lock and value.
	// Hint: Check the interfaces provided by `mvccTxn.Txn`.
	// 写入锁和数据
	keyLock = &mvcc.Lock{
		Primary: p.request.PrimaryLock,
		Ts:      txn.StartTS,
		Ttl:     p.request.LockTtl,
		Kind:    mvcc.WriteKind(mut.Op + 1),
	}
	txn.PutLock(key, keyLock)
	// 写入操作会被缓存在 writes 字段中
	switch mut.Op {
	case kvrpcpb.Op_Put:
		txn.PutValue(key, mut.Value)
	case kvrpcpb.Op_Del:
		txn.DeleteValue(key)
	}
	return nil, nil
}

func (p *Prewrite) WillWrite() [][]byte {
	result := [][]byte{}
	for _, m := range p.request.Mutations {
		result = append(result, m.Key)
	}
	return result
}
