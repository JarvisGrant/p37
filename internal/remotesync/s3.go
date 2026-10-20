package remotesync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config 是 S3 兼容后端的连接参数。凭证只从环境变量读取（见 FromEnv）。
type S3Config struct {
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	PathStyle    bool
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// S3Env 返回凭证使用的环境变量名。
const (
	EnvAccessKey = "P37_S3_ACCESS_KEY"
	EnvSecretKey = "P37_S3_SECRET_KEY"
	EnvSession   = "P37_S3_SESSION_TOKEN"
)

// S3ConfigFromEnv 读取凭证并填充配置（endpoint/bucket/prefix 仍由调用方给）。
func S3ConfigFromEnv(c S3Config) S3Config {
	c.AccessKey = os.Getenv(EnvAccessKey)
	c.SecretKey = os.Getenv(EnvSecretKey)
	c.SessionToken = os.Getenv(EnvSession)
	return c
}

// S3Remote 是 S3 兼容对象存储上的同步对端。
type S3Remote struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Remote 建立客户端。
func NewS3Remote(ctx context.Context, c S3Config) (*S3Remote, error) {
	if c.Bucket == "" {
		return nil, errors.New("remotesync: remote.bucket 不能为空")
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}
	if c.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.SessionToken)))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = c.PathStyle
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	})
	return &S3Remote{
		client: client,
		bucket: c.Bucket,
		prefix: strings.Trim(c.Prefix, "/"),
	}, nil
}

func (s *S3Remote) key(parts ...string) string {
	segs := append([]string{}, parts...)
	if s.prefix != "" {
		segs = append([]string{s.prefix}, segs...)
	}
	return path.Join(segs...)
}

func (s *S3Remote) pkgKey(id string, rest ...string) string {
	parts := append([]string{"packages", id}, rest...)
	return s.key(parts...)
}

// PutPackage 实现 Remote。
func (s *S3Remote) PutPackage(ctx context.Context, m *Manifest, srcDir string) error {
	if err := s.putFile(ctx, s.pkgKey(m.ID, MetaName), filepath.Join(srcDir, MetaName)); err != nil {
		return err
	}
	for _, p := range m.Parts {
		src, err := partPath(srcDir, p.Name)
		if err != nil {
			return err
		}
		if err := s.putFile(ctx, s.pkgKey(m.ID, p.Name), src); err != nil {
			return err
		}
	}
	return s.putFile(ctx, s.pkgKey(m.ID, ManifestName), filepath.Join(srcDir, ManifestName))
}

func (s *S3Remote) putFile(ctx context.Context, key, local string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(fi.Size()),
	})
	return err
}

// GetManifest 实现 Remote。
func (s *S3Remote) GetManifest(ctx context.Context, id string) (*Manifest, error) {
	b, err := s.getBytes(ctx, s.pkgKey(id, ManifestName))
	if err != nil {
		return nil, err
	}
	return DecodeManifest(b)
}

// GetPackage 实现 Remote。
func (s *S3Remote) GetPackage(ctx context.Context, id, dst string) (*Manifest, error) {
	m, err := s.GetManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return nil, err
	}
	if err := s.getFile(ctx, s.pkgKey(id, MetaName), filepath.Join(dst, MetaName)); err != nil {
		return nil, err
	}
	for _, p := range m.Parts {
		dstPath, err := partPath(dst, p.Name)
		if err != nil {
			return nil, err
		}
		if err := s.getFile(ctx, s.pkgKey(id, p.Name), dstPath); err != nil {
			return nil, err
		}
	}
	// 落一份本地 manifest，便于复用 VerifyDir。
	enc, err := m.Encode()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dst, ManifestName), enc, 0o600); err != nil {
		return nil, err
	}
	return m, nil
}

// ListPackages 实现 Remote。
func (s *S3Remote) ListPackages(ctx context.Context) ([]string, error) {
	prefix := s.key("packages") + "/"
	in := &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	}
	var out []string
	for {
		resp, err := s.client.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, cp := range resp.CommonPrefixes {
			if cp.Prefix == nil {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(*cp.Prefix, prefix), "/")
			if id != "" {
				out = append(out, id)
			}
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		in.ContinuationToken = resp.NextContinuationToken
	}
	return out, nil
}

// GetRef 实现 Remote。
func (s *S3Remote) GetRef(ctx context.Context) (Ref, error) {
	key := s.key("refs", "lines.json")
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return Ref{}, ErrNoRef
		}
		return Ref{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Ref{}, err
	}
	r, err := decodeRef(b)
	if err != nil {
		return Ref{}, err
	}
	if resp.ETag != nil {
		r.ETag = *resp.ETag
	}
	return r, nil
}

// UpdateRef 实现 Remote：S3 条件写（If-Match / If-None-Match）。
func (s *S3Remote) UpdateRef(ctx context.Context, expected, next Ref) error {
	key := s.key("refs", "lines.json")
	body := next.Encode()
	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}
	if expected.Head == "" {
		in.IfNoneMatch = aws.String("*")
	} else {
		// 先核对内容里的 head（ETag 只能防并发，不能表达业务语义）。
		cur, err := s.GetRef(ctx)
		if err != nil {
			if errors.Is(err, ErrNoRef) {
				return ErrRemoteMoved
			}
			return err
		}
		if cur.Head != expected.Head {
			return ErrRemoteMoved
		}
		etag := cur.ETag
		if etag == "" {
			head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(s.bucket), Key: aws.String(key),
			})
			if err != nil {
				if isNoSuchKey(err) {
					return ErrRemoteMoved
				}
				return err
			}
			if head.ETag != nil {
				etag = *head.ETag
			}
		}
		in.IfMatch = aws.String(etag)
	}
	if _, err := s.client.PutObject(ctx, in); err != nil {
		if isPreconditionFailed(err) {
			return ErrRemoteMoved
		}
		return err
	}
	return nil
}

// DeletePackage 实现 Remote。
func (s *S3Remote) DeletePackage(ctx context.Context, id string) error {
	prefix := s.pkgKey(id) + "/"
	in := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)}
	for {
		resp, err := s.client.ListObjectsV2(ctx, in)
		if err != nil {
			return err
		}
		if len(resp.Contents) > 0 {
			objs := make([]types.ObjectIdentifier, 0, len(resp.Contents))
			for _, o := range resp.Contents {
				objs = append(objs, types.ObjectIdentifier{Key: o.Key})
			}
			if _, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(s.bucket),
				Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(true)},
			}); err != nil {
				return err
			}
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		in.ContinuationToken = resp.NextContinuationToken
	}
	return nil
}

func (s *S3Remote) getBytes(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func (s *S3Remote) getFile(ctx context.Context, key, dst string) error {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func isNoSuchKey(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func isPreconditionFailed(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "PreconditionFailed", "412":
			return true
		}
	}
	return false
}

var _ Remote = (*S3Remote)(nil)
