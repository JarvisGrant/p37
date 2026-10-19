package meta

import (
	"database/sql"
	"errors"

	"github.com/chenyiwen/p37/internal/hash"
)

// VersionChain 从 head 沿 parent 往回走，返回 [head … 最老可达]。
//
// **容忍被剪枝的祖先**：`gc` 会删除最老的一批版本，于是链上会留下一个
// 指向已删除版本的 parent。这里把它当作链的终点（与「首版本」同义），
// 而不是报错 —— 否则剪枝一次之后 WalkHead / blame 就全废了。
func (d *DB) VersionChain(head hash.ID) ([]hash.ID, error) {
	out := make([]hash.ID, 0, 64)
	cur := head
	seen := 0
	for {
		v, err := d.GetVersion(cur)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				return out, nil // 剪枝边界
			}
			return nil, err
		}
		out = append(out, cur)
		if v.IsFirst() {
			return out, nil
		}
		parent, err := d.GetVersion(v.Parent)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				return out, nil // 父代已被剪枝
			}
			return nil, err
		}
		cur = parent.ID()
		seen++
		if seen > 10_000_000 {
			return nil, errors.New("meta: 版本链过长，疑似存在环")
		}
	}
}

// PruneVersion 删除一个版本及其血缘、归因缓存（§10.3）。
//
// 只删这三张表里指向它的行：
//   - version     —— 版本本身
//   - journal     —— 血缘记录（版本没了就无从索引）
//   - blame_cache —— 归属缓存（§12.2 明确要求随版本回收）
//
// op / conflict / resolution 都以 workspace 为外键，与版本无关，不动。
func (d *DB) PruneVersion(id hash.ID) error {
	s := id.String()
	return d.withWrite(func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM version WHERE id = ?`,
			`DELETE FROM journal WHERE version_id = ?`,
			`DELETE FROM blame_cache WHERE version_id = ?`,
		} {
			if _, err := tx.Exec(q, s); err != nil {
				return err
			}
		}
		return nil
	})
}

// CountObjects 返回 object_index 的行数。
func (d *DB) CountObjects() (int64, error) { return d.ObjectIndex().Count() }
