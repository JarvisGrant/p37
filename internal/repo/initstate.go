package repo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
)

// InitRequest 是一次 repo_init 的规范化请求，用于幂等冲突判定。
type InitRequest struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	Name        string `json:"name"`
	DefaultLine string `json:"default_line"`
	Force       bool   `json:"force"`
}

// Hash 返回请求的稳定哈希（canonical JSON，不含 P37 版本号）。
func (r InitRequest) Hash() string {
	b, _ := json.Marshal(r)
	return hash.Sum(b).String()
}

// InitRequestRecord 是 init.json 里按 key 保存的请求记录。
type InitRequestRecord struct {
	Mode        string `json:"mode"`
	Path        string `json:"path"`
	Name        string `json:"name"`
	DefaultLine string `json:"default_line"`
	Force       bool   `json:"force"`
	RequestHash string `json:"request_hash"`
	Version     string `json:"version,omitempty"`
	CreatedTS   int64  `json:"created_ts"`
}

type initState struct {
	Version        string                       `json:"version"`
	Path           string                       `json:"path,omitempty"`
	Mode           string                       `json:"mode"`
	Name           string                       `json:"name"`
	RepoID         string                       `json:"repo_id"`
	DefaultLine    string                       `json:"default_line"`
	Force          bool                         `json:"force,omitempty"`
	IdempotencyKey string                       `json:"idempotency_key,omitempty"`
	RequestHash    string                       `json:"request_hash,omitempty"`
	CreatedTS      int64                        `json:"created_ts"`
	Repaired       bool                         `json:"repaired,omitempty"`
	Imported       bool                         `json:"imported,omitempty"`
	Requests       map[string]InitRequestRecord `json:"requests,omitempty"`
}

func readInitState(controlDir string) (*initState, error) {
	b, err := os.ReadFile(filepath.Join(controlDir, "init.json"))
	if err != nil {
		return nil, err
	}
	var st initState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Requests == nil {
		st.Requests = map[string]InitRequestRecord{}
	}
	return &st, nil
}

func (s *initState) lookup(key string) (InitRequestRecord, bool) {
	if s == nil || key == "" || s.Requests == nil {
		return InitRequestRecord{}, false
	}
	r, ok := s.Requests[key]
	return r, ok
}

func (s *initState) put(key string, req InitRequest, version string) {
	if s.Requests == nil {
		s.Requests = map[string]InitRequestRecord{}
	}
	if key == "" {
		return
	}
	s.Requests[key] = InitRequestRecord{
		Mode: req.Mode, Path: req.Path, Name: req.Name,
		DefaultLine: req.DefaultLine, Force: req.Force,
		RequestHash: req.Hash(), Version: version, CreatedTS: time.Now().UnixMilli(),
	}
	// 只保留最近 32 个 key，避免 init.json 无限增长。
	if len(s.Requests) > 32 {
		type kv struct {
			k string
			t int64
		}
		all := make([]kv, 0, len(s.Requests))
		for k, v := range s.Requests {
			all = append(all, kv{k, v.CreatedTS})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t > all[j].t })
		for _, e := range all[32:] {
			delete(s.Requests, e.k)
		}
	}
}

func writeInitState(controlDir string, st *initState) error {
	if st.Requests == nil {
		st.Requests = map[string]InitRequestRecord{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(controlDir, "init.json"), b, 0o600)
}

// UpdateInitState 在 repo_import 等非 init 入口更新顶层版本信息，保留 key 记录。
func UpdateInitState(controlDir, path, name, defaultLine, version string, imported bool) error {
	st, err := readInitState(controlDir)
	if err != nil {
		st = &initState{Requests: map[string]InitRequestRecord{}}
	}
	if st.Requests == nil {
		st.Requests = map[string]InitRequestRecord{}
	}
	st.Version = version
	st.Path = path
	st.Name = name
	st.DefaultLine = defaultLine
	if imported {
		st.Imported = true
	}
	if st.CreatedTS == 0 {
		st.CreatedTS = time.Now().UnixMilli()
	}
	return writeInitState(controlDir, st)
}

func initConflict(key string, rec InitRequestRecord, req InitRequest) error {
	field, existing, requested := "request", "", ""
	switch {
	case rec.Path != req.Path:
		field, existing, requested = "path", rec.Path, req.Path
	case rec.Mode != req.Mode:
		field, existing, requested = "mode", rec.Mode, req.Mode
	case rec.Name != req.Name:
		field, existing, requested = "name", rec.Name, req.Name
	case rec.DefaultLine != req.DefaultLine:
		field, existing, requested = "default_line", rec.DefaultLine, req.DefaultLine
	case rec.Force != req.Force:
		field, existing, requested = "force", fmt.Sprint(rec.Force), fmt.Sprint(req.Force)
	}
	return &IdemConflictError{Key: key, Field: field, Existing: existing, Requested: requested}
}

func initStateFor(opt EnsureOptions, path, version, mode string, repaired, imported bool, prev *initState) *initState {
	st := prev
	if st == nil {
		st = &initState{}
	}
	if st.Requests == nil {
		st.Requests = map[string]InitRequestRecord{}
	}
	st.Version = version
	st.Path = path
	st.Mode = mode
	st.Name = opt.Name
	st.RepoID = opt.RepoID
	st.DefaultLine = opt.DefaultLine
	st.Force = opt.Force
	st.IdempotencyKey = opt.IdempotencyKey
	req := InitRequest{Path: path, Mode: mode, Name: opt.Name,
		DefaultLine: opt.DefaultLine, Force: opt.Force}
	st.RequestHash = req.Hash()
	if st.CreatedTS == 0 {
		st.CreatedTS = time.Now().UnixMilli()
	}
	st.Repaired = repaired
	st.Imported = imported
	st.put(opt.IdempotencyKey, req, version)
	return st
}

// InitConflict 导出请求冲突的构造，供 hub 的 root 级幂等表复用。
func InitConflict(key string, existing InitRequestRecord, req InitRequest) error {
	return initConflict(key, existing, req)
}

// InitRecordFromRequest 把请求转成记录（无 version）。
func InitRecordFromRequest(req InitRequest) InitRequestRecord {
	return InitRequestRecord{Mode: req.Mode, Path: req.Path, Name: req.Name,
		DefaultLine: req.DefaultLine, Force: req.Force, RequestHash: req.Hash()}
}
