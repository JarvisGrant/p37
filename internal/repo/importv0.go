package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

// ImportV0Result 是导入/替换初始 v0 的结果。
type ImportV0Result struct {
	VersionID    hash.ID `json:"-"`
	Version      string  `json:"version_id"`
	Imported     bool    `json:"imported"`
	Repaired     bool    `json:"repaired"`
	OldVersionID string  `json:"old_version_id,omitempty"`
	CurrentTree  string  `json:"current_tree"`
	V0Tree       string  `json:"v0_tree"`
	UserFiles    int     `json:"user_files"`
}

func repoBusy(db *meta.DB) (*BusyError, error) {
	ws, err := db.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	leases, _ := db.ListLeases()
	b := &BusyError{}
	for i := range ws {
		if ws[i].LandedTS == 0 {
			b.Workspaces = append(b.Workspaces, ws[i].ID)
		}
	}
	for _, l := range leases {
		b.Leases = append(b.Leases, l.WsID+":"+l.Path)
	}
	if len(b.Workspaces) == 0 && len(b.Leases) == 0 {
		return nil, nil
	}
	return b, nil
}

func nonDefaultLines(db *meta.DB, def string) ([]string, error) {
	lines, err := db.ListLines()
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range lines {
		if lines[i].Name != def {
			out = append(out, lines[i].Name)
		}
	}
	return out, nil
}

// ImportV0 把 srcDir（通常是 repo root）的当前内容与初始 v0 对齐。
//
// 状态机（与 repo_init(mode=import) 共用）：
//   - currentTree == v0Tree       -> 只 repair 元数据
//   - v0 为空 + current 非空      -> 直接替换 v0（无需 force）
//   - v0 非空 + current 不同/空   -> 必须 force
//   - 有历史版本/非默认 line      -> repo_history_not_empty（force 也不行）
//   - 有活跃工作区/租约           -> repo_busy（force 也不行）
func (r *Repo) ImportV0(srcDir string, force bool) (*ImportV0Result, error) {
	if srcDir == "" {
		srcDir = r.Root
	}
	st, err := r.DB.RootState()
	if err != nil {
		return nil, &DBCorruptError{Path: r.Root, Reason: "root_state_failed"}
	}
	if len(st.Roots) > 1 {
		return nil, &DBCorruptError{Path: r.Root, Reason: "multiple_root_versions"}
	}
	if st.VersionCount > 0 && len(st.Roots) == 0 {
		return nil, &DBCorruptError{Path: r.Root, Reason: "no_root_version"}
	}
	if b, err := repoBusy(r.DB); err != nil {
		return nil, err
	} else if b != nil {
		return nil, b
	}
	def, err := r.DB.DefaultLine()
	if err != nil {
		return nil, err
	}
	if def == "" {
		def = "main"
	}
	extra, err := nonDefaultLines(r.DB, def)
	if err != nil {
		return nil, err
	}
	if st.VersionCount > 1 || len(extra) > 0 {
		return nil, &HistoryNotEmptyError{Path: r.Root, VersionCount: st.VersionCount, Lines: extra}
	}

	ig := r.Ignore
	if filepath.Clean(srcDir) == filepath.Clean(r.Root) {
		if ig2, ierr := importIgnore(r.Root, Options{}); ierr == nil {
			ig = ig2
		}
	}
	copt := r.CaptureOpt
	copt.StableRetries = 0
	cur, err := capture.Build("", srcDir, ig, r.Store, nil,
		capture.InodeReliable(false), copt)
	if err != nil {
		return nil, err
	}
	if err := r.Store.Flush(); err != nil {
		return nil, err
	}
	if os.Getenv("P37_IMPORT_FAULT_AT") == "before_swap" {
		return nil, errors.New("repo: injected import fault before_swap")
	}
	empty := (&tree.Tree{}).ID()
	curID := cur.ID()

	var v0, v0Tree hash.ID
	if len(st.Roots) == 1 {
		v0 = st.Roots[0]
		ov, err := r.DB.GetVersion(v0)
		if err != nil {
			return nil, &DBCorruptError{Path: r.Root, Reason: "root_version_missing"}
		}
		v0Tree = ov.Tree
	}
	res := &ImportV0Result{
		VersionID: v0, Version: v0.String(),
		OldVersionID: v0.String(), CurrentTree: curID.String(),
		V0Tree: v0Tree.String(), UserFiles: CountUserFiles(srcDir),
	}

	repairOnly := v0Tree == curID
	replace := false
	if v0.IsZero() {
		replace = true
	} else if repairOnly {
		replace = false
	} else if v0Tree == empty && curID != empty {
		// 失败的“空初始化”留下的空 v0：直接导入，不需要 force。
		replace = true
	} else {
		if !force {
			return nil, &NotEmptyError{Path: r.Root, UserFiles: res.UserFiles,
				NextAction: "repo_init_import_force"}
		}
		replace = true
	}

	if repairOnly {
		count, err := r.DB.CountReachable(v0)
		if err != nil {
			return nil, err
		}
		if count <= 0 {
			count = 1
		}
		if err := r.DB.SetRootState(r.Root, def, v0, count, 0); err != nil {
			return nil, err
		}
		if err := r.WriteMainlineRef(v0); err != nil {
			return nil, err
		}
		res.Repaired = true
		return res, nil
	}

	if !replace {
		return nil, &NotEmptyError{Path: r.Root, UserFiles: res.UserFiles,
			NextAction: "repo_init_import_force"}
	}
	v := version.New(hash.Zero, curID, 0, AgentImporter,
		"import "+srcDir, time.Now().UnixMilli(), "import")
	journal := &meta.JournalEntry{VersionID: v.ID().String(), Agent: AgentImporter,
		TaskDesc: r.journalTask("import " + srcDir), TS: v.TS, OpsJSON: "[]"}
	fault := func(step string) error {
		if os.Getenv("P37_IMPORT_FAULT_AT") == step {
			return fmt.Errorf("repo: injected import fault %s", step)
		}
		return nil
	}
	if err := r.DB.SwapRootAtWithFault(r.Root, v0, v, def, journal, fault); err != nil {
		return nil, err
	}
	if err := fault("after_commit_before_refs"); err != nil {
		return nil, err
	}
	if err := r.WriteMainlineRef(v.ID()); err != nil {
		return nil, err
	}
	res.VersionID = v.ID()
	res.Version = v.ID().String()
	res.Imported = true
	return res, nil
}

// WriteMainlineRef 原子写 refs/mainline。
func (r *Repo) WriteMainlineRef(head hash.ID) error {
	if head.IsZero() {
		return errors.New("repo: 空 head 不能写 refs/mainline")
	}
	return r.writeHead(head)
}

// syncMainlineRefIfDefault 在默认 line head 移动后同步 refs。
func (r *Repo) syncMainlineRefIfDefault(line string, head hash.ID) error {
	def, err := r.DB.DefaultLine()
	if err != nil {
		return err
	}
	if def == "" {
		def = "main"
	}
	if line != def {
		return nil
	}
	return r.WriteMainlineRef(head)
}
