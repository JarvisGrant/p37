package remotesync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/version"
)

// MetaName 是包内元数据文件名。
const MetaName = "meta.json"

// MetaFormat 是 meta.json 的格式版本。
const MetaFormat = 1

// MetaVersion 是 version 表的一行（可审查的 snake_case 视图）。
type MetaVersion struct {
	ID        string   `json:"id"`
	Parent    string   `json:"parent"`
	Parents   []string `json:"parents,omitempty"`
	Tree      string   `json:"tree"`
	Epoch     uint64   `json:"epoch"`
	Agent     string   `json:"agent"`
	TaskHash  string   `json:"task_hash"`
	TaskDesc  string   `json:"task_desc"`
	TS        int64    `json:"ts"`
	Msg       string   `json:"msg"`
	WSID      string   `json:"ws_id"`
	JournalID int64    `json:"journal_id"`
}

// MetaJournal 是 journal 表的一行（血缘/任务描述）。
type MetaJournal struct {
	VersionID string `json:"version_id"`
	WSID      string `json:"ws_id"`
	Agent     string `json:"agent"`
	TaskDesc  string `json:"task_desc"`
	TS        int64  `json:"ts"`
	OpsJSON   string `json:"ops_json"`
}

// PackageMeta 是包内需要写入元数据库的记录。
//
// 版本与血缘是**元数据**（真相在 SQLite），不是 CAS 对象：它们随包一起传，
// 拉取时写回 version/journal 表，因此 push 到空仓库也能还原完整历史与 blame。
type PackageMeta struct {
	Format   int           `json:"format"`
	Versions []MetaVersion `json:"versions"`
	Journals []MetaJournal `json:"journals"`
}

// collectMeta 从本地库收集这些版本（从旧到新）的记录与血缘。
func collectMeta(r *repo.Repo, versionIDs []hash.ID) (*PackageMeta, error) {
	m := &PackageMeta{Format: MetaFormat}
	for _, id := range versionIDs {
		v, err := r.DB.GetVersion(id)
		if err != nil {
			return nil, fmt.Errorf("remotesync: 读取版本 %s: %w", id, err)
		}
		parents := v.Parents
		if len(parents) == 0 && !v.Parent.IsZero() {
			parents = []hash.ID{v.Parent}
		}
		pstrs := make([]string, 0, len(parents))
		for _, p := range parents {
			pstrs = append(pstrs, p.String())
		}
		mv := MetaVersion{
			ID: id.String(), Parent: v.Parent.String(), Parents: pstrs, Tree: v.Tree.String(),
			Epoch: v.Epoch, Agent: v.Agent, TaskHash: v.TaskHash.String(),
			TaskDesc: v.TaskDesc, TS: v.TS, Msg: v.Msg, WSID: v.WsID,
		}
		m.Versions = append(m.Versions, mv)
		if j, err := r.DB.GetJournal(id.String()); err == nil {
			m.Journals = append(m.Journals, MetaJournal{
				VersionID: j.VersionID, WSID: j.WsID, Agent: j.Agent,
				TaskDesc: j.TaskDesc, TS: j.TS, OpsJSON: j.OpsJSON,
			})
		}
	}
	return m, nil
}

// Encode 返回可审查的确定性 JSON。
func (m *PackageMeta) Encode() ([]byte, error) {
	if m.Versions == nil {
		m.Versions = []MetaVersion{}
	}
	if m.Journals == nil {
		m.Journals = []MetaJournal{}
	}
	return json.MarshalIndent(m, "", "  ")
}

// decodeMeta 解析 meta.json。
func decodeMeta(b []byte) (*PackageMeta, error) {
	var m PackageMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: meta.json: %v", ErrCorruptPackage, err)
	}
	if m.Format != MetaFormat {
		return nil, fmt.Errorf("%w: meta 格式 %d", ErrCorruptPackage, m.Format)
	}
	return &m, nil
}

// toVersion 把元数据行还原为 version.Version，并校验 id 自洽。
func (mv MetaVersion) toVersion() (*version.Version, error) {
	parent, err := hash.Parse(mv.Parent)
	if err != nil {
		return nil, err
	}
	tree, err := hash.Parse(mv.Tree)
	if err != nil {
		return nil, err
	}
	taskHash, err := hash.Parse(mv.TaskHash)
	if err != nil {
		return nil, err
	}
	var parents []hash.ID
	for _, ps := range mv.Parents {
		p, err := hash.Parse(ps)
		if err != nil {
			return nil, err
		}
		parents = append(parents, p)
	}
	v := &version.Version{Meta: version.Meta{
		Parent: parent, Parents: parents, Tree: tree, Epoch: mv.Epoch, Agent: mv.Agent,
		TaskHash: taskHash, TaskDesc: mv.TaskDesc, TS: mv.TS, Msg: mv.Msg,
	}, WsID: mv.WSID, JournalID: mv.JournalID}
	if v.ID().String() != mv.ID {
		return nil, fmt.Errorf("%w: 版本 %s 与内容不符（重算 %s）",
			ErrCorruptPackage, mv.ID, v.ID())
	}
	return v, nil
}

// apply 把元数据写回本地库（幂等）。
func (m *PackageMeta) apply(r *repo.Repo) error {
	for _, mv := range m.Versions {
		v, err := mv.toVersion()
		if err != nil {
			return err
		}
		if err := r.DB.InsertVersion(v); err != nil {
			return err
		}
	}
	for _, j := range m.Journals {
		if err := r.DB.InsertJournalOnce(meta.JournalEntry{
			VersionID: j.VersionID, WsID: j.WSID, Agent: j.Agent,
			TaskDesc: j.TaskDesc, TS: j.TS, OpsJSON: j.OpsJSON,
		}); err != nil {
			return err
		}
	}
	return nil
}

// readMeta 读取并校验 meta.json 的哈希。
func readMeta(dir, wantHash string) (*PackageMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, MetaName))
	if err != nil {
		return nil, err
	}
	if got := hash.Sum(b).String(); got != wantHash {
		return nil, fmt.Errorf("%w: meta.json 哈希不符", ErrCorruptPackage)
	}
	return decodeMeta(b)
}
