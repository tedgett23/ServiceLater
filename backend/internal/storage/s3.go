package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/servicelater/servicelater/backend/internal/settings"
)

// S3 keeps files in an S3-compatible bucket, uploading each as a multipart upload.
type S3 struct {
	client *minio.Client
	core   minio.Core
	bucket string
	prefix string
	direct bool
}

// NewS3 connects to the bucket the settings name, signing in with secretKey when they name an access key, else with
// the server's own credentials (AWS_* variables, ~/.aws/credentials, or the instance or pod role).
func NewS3(s settings.Storage, secretKey string) (*S3, error) {
	u, err := url.Parse(s.S3Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage: s3_endpoint %q is not an address", s.S3Endpoint)
	}
	creds := credentials.NewStaticV4(s.S3AccessKeyID, secretKey, "")
	if s.S3AccessKeyID == "" {
		creds = credentials.NewChainCredentials([]credentials.Provider{&credentials.EnvAWS{}, &credentials.FileAWSCredentials{}, &credentials.IAM{}})
	}
	lookup := minio.BucketLookupAuto
	if s.S3PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(u.Host, &minio.Options{Creds: creds, Secure: u.Scheme == "https", Region: s.S3Region, BucketLookup: lookup})
	if err != nil {
		return nil, err
	}
	return &S3{client: c, core: minio.Core{Client: c}, bucket: s.S3Bucket, prefix: s.S3Prefix, direct: s.S3Direct}, nil
}

// notFound turns S3's "no such key/upload" into ErrNotFound.
func notFound(err error) error {
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NoSuchUpload", "NotFound":
		return ErrNotFound
	}
	return err
}

func (s *S3) Begin(ctx context.Context, key, contentType string) (string, error) {
	return s.core.NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{ContentType: contentType})
}

// PutPart passes a part on as it arrives, unsigned (UNSIGNED-PAYLOAD): signing it would mean reading it twice, or
// aws-chunked framing, which not every S3-compatible service undoes.
func (s *S3) PutPart(ctx context.Context, key, uploadID string, n int, _, size int64, r io.Reader) (string, error) {
	c := &counter{r: io.LimitReader(r, size)}
	p, err := s.core.PutObjectPart(ctx, s.bucket, key, uploadID, n, c, size, minio.PutObjectPartOptions{DisableContentSha256: true})
	if c.eof && c.n < size {
		return "", ErrSize // the body ended early: whatever the transport made of it
	}
	if err != nil {
		if minio.ToErrorResponse(err).Code == "IncompleteBody" {
			return "", ErrSize
		}
		return "", notFound(err)
	}
	return p.ETag, nil
}

// counter counts what is read through it, and notices the end.
type counter struct {
	r   io.Reader
	n   int64
	eof bool
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err == io.EOF {
		c.eof = true
	}
	return n, err
}

// Complete finishes the multipart upload, then checks the object is the size the upload was said to be (parts sent
// straight to the bucket are not checked on the way), removing it when it is not.
func (s *S3) Complete(ctx context.Context, key, uploadID string, parts []Part, size int64) error {
	cp := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		cp[i] = minio.CompletePart{PartNumber: p.Number, ETag: p.ETag}
	}
	if _, err := s.core.CompleteMultipartUpload(ctx, s.bucket, key, uploadID, cp, minio.PutObjectOptions{}); err != nil {
		switch minio.ToErrorResponse(err).Code {
		case "InvalidPart", "InvalidPartOrder", "EntityTooSmall":
			return ErrSize
		}
		return notFound(err)
	}
	st, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return notFound(err)
	}
	if st.Size != size {
		_ = s.Delete(ctx, key)
		return ErrSize
	}
	return nil
}

func (s *S3) Abort(ctx context.Context, key, uploadID string) error {
	if err := notFound(s.core.AbortMultipartUpload(ctx, s.bucket, key, uploadID)); !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

type s3File struct {
	*minio.Object
	info minio.ObjectInfo
}

func (f s3File) Size() int64        { return f.info.Size }
func (f s3File) ModTime() time.Time { return f.info.LastModified }

func (s *S3) Open(ctx context.Context, key string) (File, error) {
	o, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, notFound(err)
	}
	info, err := o.Stat()
	if err != nil {
		_ = o.Close()
		return nil, notFound(err)
	}
	return s3File{o, info}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := notFound(s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})); !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

func (s *S3) Probe(ctx context.Context) error {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := s.prefix + ".servicelater-probe-" + hex.EncodeToString(b)
	if _, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{}); err != nil {
		return err
	}
	o, err := s.Open(ctx, key)
	if err == nil {
		var got []byte
		got, err = io.ReadAll(o)
		_ = o.Close()
		if err == nil && !bytes.Equal(got, b) {
			err = errors.New("storage: an object read back differs from what was written")
		}
	}
	return errors.Join(err, s.Delete(ctx, key))
}

// Direct says whether browsers upload to and download from the bucket themselves.
func (s *S3) Direct() bool { return s.direct }

func (s *S3) PartURL(ctx context.Context, key, uploadID string, n int, expiry time.Duration) (string, error) {
	u, err := s.client.Presign(ctx, http.MethodPut, s.bucket, key, expiry,
		url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *S3) DownloadURL(ctx context.Context, key, filename, contentType string, inline bool, expiry time.Duration) (string, error) {
	ct, disp := Headers(filename, contentType, inline)
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, expiry,
		url.Values{"response-content-type": {ct}, "response-content-disposition": {disp}})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// CORSProblems says what in the bucket's CORS rules would stop a browser at origin uploading straight to it: it
// must allow PUT and GET from the origin, and expose ETag (the browser reports each part's ETag to finish).
func (s *S3) CORSProblems(ctx context.Context, origin string) []string {
	c, err := s.client.GetBucketCors(ctx, s.bucket)
	if err != nil {
		return []string{fmt.Sprintf("Could not read the bucket's CORS rules (%v). Browsers upload straight to the bucket, so it must allow this site.", err)}
	}
	if c == nil {
		return []string{"The bucket has no CORS rules, so browsers cannot upload to it from this site."}
	}
	for _, r := range c.CORSRules {
		if matchOrigin(r.AllowedOrigin, origin) && has(r.AllowedMethod, "PUT") && has(r.AllowedMethod, "GET") {
			if !has(r.ExposeHeader, "ETag") {
				return []string{"The bucket's CORS rule for this site must expose the ETag header."}
			}
			return nil
		}
	}
	return []string{fmt.Sprintf("No CORS rule on the bucket allows PUT and GET from %s.", origin)}
}

func has(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) || x == "*" {
			return true
		}
	}
	return false
}

func matchOrigin(allowed []string, origin string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
		if i := strings.Index(a, "*"); i >= 0 && strings.HasPrefix(origin, a[:i]) && strings.HasSuffix(origin, a[i+1:]) {
			return true
		}
	}
	return false
}
