package repo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
)

// Integrity 是仓库初始化完整性的一次只读快照。
//
// 这是 repo_register / repo_health / p37 doctor / repo_scan 的共同判定，
// 避免各处对“什么算初始化完成”各写一套。
type Integrity struct {
	Initialized      bool   `json:"initialized"`
	Reason           string `json:"reason,omitempty"`
	NextAction       string `json:"next_action,omitempty"`
	Path             string `json:"path"`
	Root             string `json:"root,omitempty"`
	DefaultLine      string `json:"default_line,omitempty"`
	DefaultLineReady bool   `json:"default_line_ready"`
	MainlineHead     string `json:"mainline_head,omitempty"`
	VersionCount     int64  `json:"version_count"`
	RepoDBOK         bool   `json:"repo_db_ok"`
	DataOK           bool   `json:"data_ok"`
	RepoJSONOK       bool   `json:"repo_json_ok"`
	RootOK           bool   `json:"root_ok"`
	RefsOK           bool   `json:"refs_ok"`
	RefsMatch        bool   `json:"refs_match"`
	RefsHead         string `json:"refs_head,omitempty"`
	FormatOK         bool   `json:"format_ok"`
	FormatVer        int    `json:"format_ver"`
	HeadVersionOK    bool   `json:"head_version_ok"`
	TreeOK           bool   `json:"tree_ok"`
	// UserFiles 只在缺 DB 时统计（用于决定 repo_init 还是 repo_init_import）。
	UserFiles  int    `json:"user_files,omitempty"`
	BackupPath string `json:"backup_path,omitempty"`
}

// CheckIntegrity 只读探测一个仓库目录。
//
// 不建表、不跑迁移、不拿写锁。deep=true 时还会检查 head 的 tree 对象。
func CheckIntegrity(root string) (*Integrity, error) {
	root = filepath.Clean(root)
	in := &Integrity{Path: root}
	p37 := filepath.Join(root, ".p37")
	dbPath := filepath.Join(p37, "p37.db")
	if _, err := os.Stat(p37); err != nil || !fileExists(dbPath) {
		in.UserFiles = CountUserFiles(root)
		if in.UserFiles > 0 {
			in.NextAction = "repo_init_import"
			in.Reason = "repo_db_missing_with_user_files"
		} else {
			in.NextAction = "repo_init"
			in.Reason = "repo_db_missing_or_corrupt"
		}
		return in, nil
	}

	// repo.json 不是初始化判定的一部分（老仓库可能没有），但它决定身份锚是否完整。
	if m, err := ReadMeta(root); err == nil && m != nil {
		in.RepoJSONOK = true
	}

	db, err := meta.OpenReadOnly(root)
	if err != nil {
		in.Reason = "repo_db_missing_or_corrupt"
		in.NextAction = "repo_init_repair"
		in.RepoDBOK = false
		return in, nil
	}
	defer func() { _ = db.Close() }()
	in.RepoDBOK = true
	if v, err := db.UserVersion(); err == nil {
		in.FormatVer = v
		in.FormatOK = v == meta.FormatVer
	}
	if err := evaluateIntegrity(in, db, root, false); err != nil {
		// 能打开但读不出结构：按损坏处理，不在 health 里改库。
		in.Initialized = false
		in.Reason = "repo_db_unreadable"
		in.NextAction = "repo_init_repair"
		return in, nil
	}
	return in, nil
}

// CheckIntegrityDeep 是 CheckIntegrity 的完整版：额外检查对象树。
func CheckIntegrityDeep(root string) (*Integrity, error) {
	in, err := CheckIntegrity(root)
	if err != nil || !in.Initialized {
		return in, err
	}
	if !fileExists(filepath.Join(root, ".p37", "p37.db")) {
		return in, nil
	}
	db, err := meta.OpenReadOnly(root)
	if err != nil {
		in.Initialized = false
		in.Reason = "repo_db_missing_or_corrupt"
		in.NextAction = "repo_init_repair"
		return in, nil
	}
	defer func() { _ = db.Close() }()
	if err := evaluateIntegrity(in, db, root, true); err != nil {
		in.Initialized = false
		in.Reason = "tree_object_missing"
		in.NextAction = "repo_init_repair"
	}
	return in, nil
}

// IntegrityOpen 对已打开的 repo 做同样的判定（复用连接，不重新打开 DB）。
func IntegrityOpen(r *Repo) *Integrity {
	in := &Integrity{Path: r.Root, RepoDBOK: true}
	if _, err := ReadMeta(r.Root); err == nil {
		in.RepoJSONOK = true
	}
	if v, err := r.DB.UserVersion(); err == nil {
		in.FormatVer = v
		in.FormatOK = v == meta.FormatVer
	}
	if err := evaluateIntegrity(in, r.DB, r.Root, true); err != nil {
		in.Initialized = false
		in.Reason = "repo_db_unreadable"
		in.NextAction = "repo_init_repair"
	}
	return in
}

func evaluateIntegrity(in *Integrity, db *meta.DB, root string, deep bool) error {
	st, err := db.RootState()
	if err != nil {
		return err
	}
	in.Root = st.Repo.Root
	in.DefaultLine = st.Repo.DefaultLine
	if in.DefaultLine == "" {
		in.DefaultLine = "main"
	}
	in.RootOK = filepath.Clean(st.Repo.Root) == filepath.Clean(root)
	in.MainlineHead = st.Repo.MainlineHead.String()
	in.VersionCount = st.Repo.VersionCount

	head := st.Repo.MainlineHead
	headVersionOK := false
	if !head.IsZero() {
		if _, err := db.GetVersion(head); err == nil {
			headVersionOK = true
		}
	}
	in.HeadVersionOK = headVersionOK

	var defLine *meta.Line
	for i := range st.Lines {
		if st.Lines[i].Name == in.DefaultLine {
			defLine = &st.Lines[i]
			break
		}
	}
	lineHeadOK := false
	if defLine != nil && !defLine.Head.IsZero() {
		if _, err := db.GetVersion(defLine.Head); err == nil {
			lineHeadOK = true
		}
	}
	in.DefaultLineReady = defLine != nil && lineHeadOK

	refsOK := false
	if b, err := os.ReadFile(filepath.Join(root, ".p37", "refs", "mainline")); err == nil {
		ref := strings.TrimSpace(string(b))
		in.RefsHead = ref
		id, perr := hash.Parse(ref)
		if perr == nil && !id.IsZero() {
			if _, verr := db.GetVersion(id); verr == nil {
				refsOK = true
			}
		}
		// refs 必须和 DB 的 mainline_head 一致，否则 Open/land 会分叉。
		if refsOK && !head.IsZero() {
			in.RefsMatch = id == head
		}
	}
	in.RefsOK = refsOK

	in.DataOK = in.RepoDBOK && in.FormatOK &&
		!head.IsZero() && headVersionOK && in.VersionCount > 0 &&
		in.DefaultLineReady && refsOK
	in.Initialized = in.DataOK && in.RootOK

	if deep && in.Initialized {
		if bs, err := cas.OpenLocalFSReadOnly(filepath.Join(root, ".p37")); err == nil {
			store := cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex())
			if v, err := store.GetVersion(head); err == nil {
				if _, err := store.GetTree(v.Tree); err != nil {
					in.TreeOK = false
				} else {
					in.TreeOK = true
				}
			}
		}
	} else {
		in.TreeOK = in.Initialized
	}

	if in.Initialized {
		in.Reason = ""
		in.NextAction = ""
		if !in.RepoJSONOK {
			// 数据可用，但身份锚缺失：注册时补写，不阻塞 /ready。
			in.NextAction = "repo_init_repair"
		}
		return nil
	}

	switch {
	case !in.FormatOK:
		in.Reason = "format_mismatch"
		in.NextAction = "repo_init_repair"
	case !in.RootOK:
		in.Reason = "repo_root_mismatch"
		in.NextAction = "repo_init_repair"
	case head.IsZero():
		in.Reason = "mainline_head_missing"
		in.NextAction = "repo_init_repair"
	case !headVersionOK:
		in.Reason = "head_version_missing"
		in.NextAction = "repo_init_repair"
	case st.Repo.VersionCount <= 0:
		in.Reason = "version_count_zero"
		in.NextAction = "repo_init_repair"
	case defLine == nil:
		in.Reason = "default_line_missing"
		in.NextAction = "repo_init_repair"
	case !lineHeadOK:
		in.Reason = "default_line_head_missing"
		in.NextAction = "repo_init_repair"
	case !refsOK:
		if in.Reason == "" {
			in.Reason = "refs_missing_or_mismatch"
		}
		in.NextAction = "repo_init_repair"
	default:
		if in.Reason == "" {
			in.Reason = "repo_not_initialized"
		}
		in.NextAction = "repo_init_repair"
	}
	return nil
}

// CountUserFiles 统计目录里的“用户文件”，排除 p37 自己的控制目录。
//
// 统计到 1000 即停止：调用方只需要判断是否非空，不需要精确全量。
func CountUserFiles(root string) int {
	const limit = 1000
	n := 0
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if n >= limit || depth > 32 {
			return
		}
		des, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, de := range des {
			if isControlEntry(de.Name()) {
				continue
			}
			n++
			if n >= limit {
				return
			}
			if de.IsDir() {
				walk(filepath.Join(dir, de.Name()), depth+1)
			}
		}
	}
	if len(entries) > 0 {
		walk(root, 0)
	}
	return n
}

// ControlEntries 是初始化时始终排除的仓库控制项。
var ControlEntries = []string{
	".p37", ".p37.tmp", ".p37.init.lock", ".p37.lock",
	"workspaces", "lost+found",
}

func isControlEntry(name string) bool {
	for _, c := range ControlEntries {
		if name == c {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Error helpers for callers that need to distinguish integrity outcomes.
var (
	// ErrDBMissing 表示 .p37/p37.db 不存在。
	ErrDBMissing = errors.New("repo: 元数据库不存在")
	// ErrDBCorrupt 表示元数据库无法读取或结构不可恢复。
	ErrDBCorrupt = errors.New("repo: 元数据库损坏")
)
