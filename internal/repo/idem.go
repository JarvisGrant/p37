package repo

import (
	"encoding/json"
	"fmt"
	"time"
)

// landIdemTTL 比版本幂等键的 7 天更长：即使工作区已经 drop、7 天窗口过去，
// 同一个 key 仍能拿到首次落地结果。
const landIdemTTL = 30 * 24 * time.Hour

type idemEnvelope struct {
	Request map[string]string `json:"request"`
	Result  json.RawMessage   `json:"result"`
}

func (r *Repo) putLandIdem(tool, key string, req map[string]string, res *LandResult) {
	if key == "" {
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		return
	}
	env, err := json.Marshal(idemEnvelope{Request: req, Result: b})
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	_ = r.DB.PutIdempotencyResult(tool, key, string(env), now,
		now+landIdemTTL.Milliseconds())
}

// getLandIdem 读取首次 land 结果。兼容老记录（直接是 LandResult JSON）。
func (r *Repo) getLandIdem(tool, key string) (map[string]string, *LandResult, bool) {
	if key == "" {
		return nil, nil, false
	}
	rec, err := r.DB.GetIdempotencyResult(tool, key, time.Now().UnixMilli())
	if err != nil || rec == nil {
		return nil, nil, false
	}
	var env idemEnvelope
	if err := json.Unmarshal([]byte(rec.ResultJSON), &env); err == nil && len(env.Result) > 0 {
		var res LandResult
		if err := json.Unmarshal(env.Result, &res); err == nil {
			return env.Request, &res, true
		}
	}
	var res LandResult
	if err := json.Unmarshal([]byte(rec.ResultJSON), &res); err == nil {
		return nil, &res, true
	}
	return nil, nil, false
}

func checkLandIdem(req map[string]string, stored map[string]string) error {
	if len(stored) == 0 {
		return nil
	}
	for _, k := range []string{"ws_id", "target", "fold", "parent_ws_id"} {
		if want, ok := stored[k]; ok && want != "" && req[k] != want {
			return &IdemConflictError{
				Key: req["idempotency_key"], Field: k,
				Existing:  fmt.Sprintf("%s=%s", k, want),
				Requested: fmt.Sprintf("%s=%s", k, req[k]),
			}
		}
	}
	return nil
}

// LandHistoryEntry 是一次 land 的持久化审计记录（30 天）。
type LandHistoryEntry struct {
	Tool      string            `json:"tool"`
	Key       string            `json:"idempotency_key"`
	Request   map[string]string `json:"request,omitempty"`
	Result    *LandResult       `json:"result,omitempty"`
	CreatedTS int64             `json:"created_ts"`
	ExpiresTS int64             `json:"expires_ts,omitempty"`
}

// LandHistory 读取 ws_land / ws_land_into 的幂等记录，按时间倒序。
//
// 用于事后复盘：某次 land 到底写了哪些路径、父树前后是什么、冲突挂在谁身上。
// wsID 非空时只返回该工作区的记录。
func (r *Repo) LandHistory(wsID string, limit int) ([]LandHistoryEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []LandHistoryEntry
	for _, tool := range []string{"ws_land", "ws_land_into"} {
		recs, err := r.DB.ListKVAll(tool, limit)
		if err != nil {
			return nil, err
		}
		for i := range recs {
			rec := &recs[i]
			ent := LandHistoryEntry{Tool: tool, Key: rec.Key,
				CreatedTS: rec.CreatedTS, ExpiresTS: rec.ExpiresTS}
			var env idemEnvelope
			if err := json.Unmarshal([]byte(rec.Value), &env); err == nil && len(env.Result) > 0 {
				ent.Request = env.Request
				var res LandResult
				if err := json.Unmarshal(env.Result, &res); err == nil {
					ent.Result = &res
				}
			} else {
				var res LandResult
				if err := json.Unmarshal([]byte(rec.Value), &res); err == nil {
					ent.Result = &res
				}
			}
			if wsID != "" && (ent.Request == nil || ent.Request["ws_id"] != wsID) {
				continue
			}
			out = append(out, ent)
		}
	}
	return out, nil
}
