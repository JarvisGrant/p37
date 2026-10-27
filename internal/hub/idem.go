package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/chenyiwen/p37/internal/repo"
)

const initKeyLease = 5 * time.Minute

type initClaim struct {
	Done   bool
	Owner  bool
	Result *repo.EnsureResult
}

// claimInitKey 在 root registry 中原子 claim 一个 init 幂等键。
func (h *Hub) claimInitKey(key string, req repo.InitRequest) (*initClaim, error) {
	if key == "" {
		return &initClaim{Owner: true}, nil
	}
	now := time.Now().UnixMilli()
	lease := now + initKeyLease.Milliseconds()
	hash := req.Hash()
	tx, err := h.db.Begin()
	if err != nil {
		return nil, wrapRegistryErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		path, mode, name, def, reqHash, status, resultJSON string
		force                                              int
		updated, leaseUntil                                int64
	)
	err = tx.QueryRow(
		`SELECT path, mode, name, default_line, force, request_hash, status,
		        result_json, updated_ts, lease_until
		   FROM init_idempotency WHERE key=?`, key).
		Scan(&path, &mode, &name, &def, &force, &reqHash, &status, &resultJSON,
			&updated, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec(
			`INSERT INTO init_idempotency(key,path,mode,name,default_line,force,
			  request_hash,status,result_json,created_ts,updated_ts,lease_until)
			 VALUES(?,?,?,?,?,?,?,'in_progress','',?,?,?)`,
			key, req.Path, req.Mode, req.Name, req.DefaultLine, boolInt(req.Force),
			hash, now, now, lease)
		if err != nil {
			return nil, wrapRegistryErr(err)
		}
		if err := tx.Commit(); err != nil {
			return nil, wrapRegistryErr(err)
		}
		return &initClaim{Owner: true}, nil
	}
	if err != nil {
		return nil, wrapRegistryErr(err)
	}

	existing := repo.InitRequestRecord{Path: path, Mode: mode, Name: name,
		DefaultLine: def, Force: force != 0, RequestHash: reqHash}
	if reqHash != hash {
		return nil, repo.InitConflict(key, existing, req)
	}
	switch status {
	case "done":
		var res repo.EnsureResult
		if resultJSON != "" {
			if err := json.Unmarshal([]byte(resultJSON), &res); err != nil {
				return nil, err
			}
		}
		return &initClaim{Done: true, Result: &res}, nil
	case "in_progress":
		if leaseUntil > 0 && leaseUntil < now {
			// 上次进程崩溃留下的过期 claim：同 request 可以接管。
			if err := updateInitClaim(tx, key, hash, lease); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, wrapRegistryErr(err)
			}
			return &initClaim{Owner: true}, nil
		}
		return &initClaim{}, nil
	case "failed":
		// 失败后同 request 允许重试；保留 request_hash 让不同 request 继续冲突。
		if err := updateInitClaim(tx, key, hash, lease); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, wrapRegistryErr(err)
		}
		return &initClaim{Owner: true}, nil
	default:
		return &initClaim{}, nil
	}
}

func updateInitClaim(tx *sql.Tx, key, hash string, lease int64) error {
	now := time.Now().UnixMilli()
	_, err := tx.Exec(
		`UPDATE init_idempotency SET request_hash=?, status='in_progress',
		        result_json='', updated_ts=?, lease_until=? WHERE key=?`,
		hash, now, lease, key)
	return wrapRegistryErr(err)
}

func (h *Hub) completeInitKey(key, hash string, res *repo.EnsureResult) {
	if key == "" || res == nil {
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	_, _ = h.db.Exec(
		`UPDATE init_idempotency SET status='done', result_json=?, request_hash=?,
		        updated_ts=?, lease_until=0 WHERE key=?`,
		string(b), hash, now, key)
}

func (h *Hub) failInitKey(key string) {
	if key == "" {
		return
	}
	_, _ = h.db.Exec(
		`UPDATE init_idempotency SET status='failed', updated_ts=?, lease_until=0 WHERE key=?`,
		time.Now().UnixMilli(), key)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
