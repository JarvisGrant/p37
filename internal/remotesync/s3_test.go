package remotesync_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/chenyiwen/p37/internal/remotesync"
)

// fakeS3 是测试用的最小 S3（path-style）：Put/Get/Head/List/Delete。
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte // key -> body
}

func newFakeS3() *fakeS3 { return &fakeS3{objs: map[string][]byte{}} }

func (f *fakeS3) etag(key string) string {
	sum := md5.Sum(f.objs[key])
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// path: /<bucket>/<key>
	p := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(p, "/", 2)
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}

	q := r.URL.Query()
	if r.Method == http.MethodGet && q.Get("list-type") == "2" {
		f.list(w, q)
		return
	}
	if r.Method == http.MethodPost && q.Has("delete") {
		f.deleteMany(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		if im := r.Header.Get("If-Match"); im != "" {
			if _, ok := f.objs[key]; !ok || im != f.etag(key) {
				http.Error(w, "precondition failed", http.StatusPreconditionFailed)
				return
			}
		}
		if inm := r.Header.Get("If-None-Match"); inm == "*" {
			if _, ok := f.objs[key]; ok {
				http.Error(w, "precondition failed", http.StatusPreconditionFailed)
				return
			}
		}
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
		w.Header().Set("ETag", f.etag(key))
		w.WriteHeader(200)
	case http.MethodGet, http.MethodHead:
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		w.Header().Set("ETag", f.etag(key))
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(204)
	default:
		http.Error(w, "unsupported", 405)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, q url.Values) {
	prefix := q.Get("prefix")
	delim := q.Get("delimiter")
	seen := map[string]bool{}
	var contents []string
	var common []string
	for k, v := range f.objs {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if delim != "" {
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.Index(rest, delim); i >= 0 {
				cp := prefix + rest[:i+len(delim)]
				if !seen[cp] {
					seen[cp] = true
					common = append(common, cp)
				}
				continue
			}
		}
		contents = append(contents, fmt.Sprintf(
			`<Contents><Key>%s</Key><ETag>%s</ETag><Size>%d</Size></Contents>`,
			xmlEscape(k), f.etag(k), len(v)))
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	b.WriteString("<IsTruncated>false</IsTruncated>")
	b.WriteString(fmt.Sprintf("<Name>b</Name><Prefix>%s</Prefix>", xmlEscape(prefix)))
	b.WriteString(fmt.Sprintf("<KeyCount>%d</KeyCount>", len(contents)+len(common)))
	for _, c := range contents {
		b.WriteString(c)
	}
	for _, cp := range common {
		b.WriteString("<CommonPrefixes><Prefix>" + xmlEscape(cp) + "</Prefix></CommonPrefixes>")
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func (f *fakeS3) deleteMany(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = xml.Unmarshal(body, &req)
	for _, o := range req.Objects {
		delete(f.objs, o.Key)
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(`<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func newTestS3Remote(t *testing.T) (*remotesync.S3Remote, *fakeS3) {
	t.Helper()
	f := newFakeS3()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	remote, err := remotesync.NewS3Remote(context.Background(), remotesync.S3Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "b", Prefix: "repo",
		PathStyle: true, AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatal(err)
	}
	return remote, f
}

// TestS3RemoteContract 覆盖 S3 口的 Put/Get/List/Ref/Delete。
func TestS3RemoteContract(t *testing.T) {
	ctx := context.Background()
	remote, f := newTestS3Remote(t)

	// 先造一个包。
	repoA, _ := newRepo(t)
	importFiles(t, repoA, map[string]string{"a.txt": "s3\n"})
	o := opts(t)
	m, err := remotesync.Prepare(repoA, o, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.PutPackage(ctx, m, mDir(o, m.ID)); err != nil {
		t.Fatal(err)
	}

	ids, err := remote.ListPackages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != m.ID {
		t.Fatalf("ListPackages = %v", ids)
	}
	got, err := remote.GetManifest(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != m.ID || got.ResultVersion != m.ResultVersion {
		t.Fatalf("manifest 不一致: %+v", got)
	}
	dst := t.TempDir()
	if _, err := remote.GetPackage(ctx, m.ID, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.VerifyDir(dst); err != nil {
		t.Fatal(err)
	}

	// ref：创建 -> 条件更新 -> 冲突。
	next := remotesync.Ref{Head: m.ResultVersion, Package: m.ID, TS: 1}
	if err := remote.UpdateRef(ctx, remotesync.Ref{}, next); err != nil {
		t.Fatal(err)
	}
	ref, err := remote.GetRef(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Head != m.ResultVersion || ref.ETag == "" {
		t.Fatalf("GetRef = %+v", ref)
	}
	if err := remote.UpdateRef(ctx, remotesync.Ref{Head: "deadbeef"}, next); err != remotesync.ErrRemoteMoved {
		t.Fatalf("期望 ErrRemoteMoved，得到 %v", err)
	}
	if err := remote.DeletePackage(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objs["repo/packages/"+m.ID+"/manifest.json"]; ok {
		t.Fatal("DeletePackage 未删除对象")
	}
}

func mDir(o remotesync.Options, id string) string {
	return o.Outbox + "/" + id
}

// TestS3PushPull 端到端：S3 口上推一个 checkpoint，再拉进空仓库。
func TestS3PushPull(t *testing.T) {
	ctx := context.Background()
	remote, _ := newTestS3Remote(t)

	a, _ := newRepo(t)
	importFiles(t, a, map[string]string{"a.txt": "s3-e2e\n"})
	land(t, a, "v1", map[string]string{"a.txt": "s3-e2e-2\n"})
	o := opts(t)
	if _, err := remotesync.Prepare(a, o, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.Push(ctx, a, remote, nil); err != nil {
		t.Fatal(err)
	}

	b, _ := newRepo(t)
	res, err := remotesync.Pull(ctx, b, remote, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.FastForward {
		t.Fatalf("应快进: %+v", res)
	}
	if got := readAt(t, b, mustHead(t, b), "a.txt"); got != "s3-e2e-2\n" {
		t.Fatalf("a.txt = %q", got)
	}
}
