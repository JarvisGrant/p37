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
