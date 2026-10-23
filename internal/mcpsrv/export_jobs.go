package mcpsrv

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/chenyiwen/p37/internal/repo"
)

// exportJob 是一次异步 repo_export 的进程内状态。
//
// 任务不持久化：daemon 重启后 job_id 失效，调用方应重新发起导出，
// 并清理可能已部分写入的目录。
type exportJob struct {
	ID         string
	Repo       string
	Ref        string
	DestDir    string
	State      string // running | done | failed
	StartedTS  int64
	FinishedTS int64
	Result     *repo.ExportResult
	Err        *ErrObj
}

const exportJobTTL = time.Hour

func newExportJobID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "exp_" + hex.EncodeToString(b[:]), nil
}

func (s *Server) startExportJob(r *repo.Repo, repoName, ref, dest string) (string, string) {
	now := nowMS()
	s.exportMu.Lock()
	s.pruneExportJobsLocked(now)
	id, err := newExportJobID()
	if err != nil {
		// crypto/rand 失败极罕见；退化为时间戳，避免把内部错误塞进
		// 异步任务创建路径。
		id = "exp_" + hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	job := &exportJob{
		ID: id, Repo: repoName, Ref: ref, DestDir: dest,
		State: "running", StartedTS: now,
	}
	s.exportJobs[id] = job
	s.exportMu.Unlock()

	go func() {
		res, err := r.Export(ref, dest)
		s.exportMu.Lock()
		defer s.exportMu.Unlock()
		job.FinishedTS = nowMS()
		if err != nil {
			code, msg, detail := classify(err)
			job.State = "failed"
			job.Err = &ErrObj{Code: code, Message: msg, Detail: detail}
			return
		}
		job.State = "done"
		job.Result = res
	}()
	return id, "running"
}

func (s *Server) exportJobStatus(id string) (any, error) {
	if id == "" {
		return nil, coded("invalid_argument", "缺少 job_id")
	}
	now := nowMS()
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	s.pruneExportJobsLocked(now)
	job := s.exportJobs[id]
	if job == nil {
		return nil, coded("not_found", "导出任务 %q 不存在或已过期", id)
	}
	out := map[string]any{
		"job_id": job.ID, "repo": job.Repo, "ref": job.Ref,
		"dest_dir": job.DestDir, "state": job.State,
		"started_ts": job.StartedTS,
	}
	if job.FinishedTS != 0 {
		out["finished_ts"] = job.FinishedTS
	}
	if job.Result != nil {
		out["version_id"] = job.Result.VersionID.String()
		out["files"] = job.Result.Files
		out["bytes"] = job.Result.Bytes
		out["exported"] = true
	}
	if job.Err != nil {
		out["error"] = job.Err
	}
	return out, nil
}

// pruneExportJobsLocked 删除完成超过 TTL 的任务。调用方必须持有 exportMu。
func (s *Server) pruneExportJobsLocked(now int64) {
	if s.exportJobs == nil {
		s.exportJobs = map[string]*exportJob{}
		return
	}
	cutoff := now - exportJobTTL.Milliseconds()
	for id, job := range s.exportJobs {
		if job.FinishedTS != 0 && job.FinishedTS < cutoff {
			delete(s.exportJobs, id)
		}
	}
}
