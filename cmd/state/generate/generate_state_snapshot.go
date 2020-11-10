package generate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/core/state"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/turbo/torrent"
	"github.com/ledgerwatch/turbo-geth/turbo/trie"
	"os"
	"os/signal"
	"time"
)

//  ./build/bin/tg --datadir /media/b00ris/nvme/snapshotsync/ --nodiscover --snapshot-mode hb --port 30304
// go run ./cmd/state/main.go stateSnapshot --block 11000000 --chaindata /media/b00ris/nvme/tgstaged/tg/chaindata/ --snapshot /media/b00ris/nvme/snapshots/state
func GenerateStateSnapshot(dbPath, snapshotPath string, toBlock uint64, snapshotDir string, snapshotMode string) error {
	if snapshotPath == "" {
		return errors.New("empty snapshot path")
	}

	err := os.RemoveAll(snapshotPath)
		if err != nil {
		return err
	}
	kv := ethdb.NewLMDB().Path(dbPath).MustOpen()

	if snapshotDir != "" {
		var mode torrent.SnapshotMode
		mode, err = torrent.SnapshotModeFromString(snapshotMode)
		if err != nil {
			return err
		}

		kv, err = torrent.WrapBySnapshots(kv, snapshotDir, mode)
		if err != nil {
			return err
		}
	}
	snkv := ethdb.NewLMDB().WithBucketsConfig(func(defaultBuckets dbutils.BucketsCfg) dbutils.BucketsCfg {
		return dbutils.BucketsCfg{
		dbutils.PlainStateBucket:       dbutils.BucketConfigItem{},
		dbutils.PlainContractCodeBucket:       dbutils.BucketConfigItem{},
		dbutils.CodeBucket:       dbutils.BucketConfigItem{},
		dbutils.SnapshotInfoBucket: dbutils.BucketConfigItem{},
	}
	}).Path(snapshotPath).MustOpen()

	ch := make(chan os.Signal, 1)
	quitCh := make(chan struct{})
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		close(quitCh)
	}()

	//db := ethdb.NewObjectDatabase(kv)
	sndb := ethdb.NewObjectDatabase(snkv)
	mt:=sndb.NewBatch()

	tx, err := kv.Begin(context.Background(), nil, false)
	if err != nil {
		return err
	}
	tx2, err := kv.Begin(context.Background(), nil, false)
	if err != nil {
		return err
	}
	defer tx.Rollback()


	i:=0
	t:=time.Now()
	tt:=time.Now()
	//st:=0
	//var emptyCodeHash = crypto.Keccak256Hash(nil)
	err = state.WalkAsOf(tx, dbutils.PlainStateBucket,dbutils.AccountsHistoryBucket, []byte{},0,toBlock+1, func(k []byte, v []byte) (bool, error) {
		i++
		if i%10000==0 {
			fmt.Println(i, common.Bytes2Hex(k),"batch", time.Since(tt))
			tt=time.Now()
			select {
			case <-ch:
				return false, errors.New("interrupted")
			default:

			}
		}
		if len(k) != 20 {
			fmt.Println("ln", len(k))
			return true, nil
		}
		var acc accounts.Account
		if err = acc.DecodeForStorage(v); err != nil {
			return false, fmt.Errorf("decoding %x for %x: %v", v, k, err)
		}

		if acc.Incarnation>0 {
			t := trie.New(common.Hash{})

			storagePrefix := dbutils.PlainGenerateStoragePrefix(k, acc.Incarnation)
			innerErr := state.WalkAsOf(tx2, dbutils.PlainStateBucket, dbutils.StorageHistoryBucket, storagePrefix, 8*(common.AddressLength+common.IncarnationLength), toBlock+1, func(kk []byte, vv []byte) (bool, error) {
				if !bytes.Equal(kk[:common.AddressLength], k) {
					fmt.Println("k", common.Bytes2Hex(k), "kk",common.Bytes2Hex(k))
				}
				innerErr1:=mt.Put(dbutils.PlainStateBucket, dbutils.PlainGenerateCompositeStorageKey(common.BytesToAddress(kk[:common.AddressLength]),acc.Incarnation, common.BytesToHash(kk[common.AddressLength:])), common.CopyBytes(vv))
				if innerErr1!=nil {
					fmt.Println("mt.Put", innerErr1)
					return false, innerErr1
				}

				h, _ := common.HashData(kk[common.AddressLength:])
				t.Update(h.Bytes(), common.CopyBytes(vv))

				return true, nil
			})
			if innerErr!=nil {
				fmt.Println("Storage walkasof")
				return false, innerErr
			}

			var codeHash []byte
			codeHash, err = tx2.Get(dbutils.PlainContractCodeBucket, storagePrefix)
			if err != nil && err != ethdb.ErrKeyNotFound {
				return false, fmt.Errorf("getting code hash for %x: %v", k, err)
			}
			err:=mt.Put(dbutils.PlainContractCodeBucket, storagePrefix, codeHash)
			if err!=nil {
				return false, err
			}

			if len(codeHash)>0 {
				var code []byte
				if code, err = tx2.Get(dbutils.CodeBucket, codeHash); err != nil {
					fmt.Println("tx.Get(dbutils.CodeBucket")
					return false, err
				}
				if err := mt.Put(dbutils.CodeBucket, codeHash, code); err != nil {
					fmt.Println("mt.Put(dbutils.CodeBucket")
					return false, err
				}

			}

			acc.Root = t.Hash()
		}
		newAcc:=make([]byte, acc.EncodingLengthForStorage())
		acc.EncodeForStorage(newAcc)
		innerErr:=mt.Put(dbutils.PlainStateBucket, common.CopyBytes(k), newAcc)
		if innerErr!=nil {
			return false, innerErr
		}

		if mt.BatchSize() >= mt.IdealBatchSize() {
			ttt:=time.Now()
			innerErr = mt.CommitAndBegin(context.Background())
			if innerErr!=nil {
				fmt.Println("mt.BatchSize", innerErr)
				return false, innerErr
			}
			fmt.Println("Commited", time.Since(ttt))
		}
		return true, nil
	})
	if err!=nil {
		return err
	}
	_,err=mt.Commit()
	if err!=nil {
		return err
	}
	fmt.Println("took", time.Since(t))

	return err
}