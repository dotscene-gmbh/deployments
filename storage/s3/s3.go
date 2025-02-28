// Copyright 2023 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package s3

import (
	"bytes"
	"context"
	stderr "errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsHttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/pkg/errors"

	"github.com/mendersoftware/deployments/model"
	"github.com/mendersoftware/deployments/storage"
)

const (
	ExpireMaxLimit = 7 * 24 * time.Hour
	ExpireMinLimit = 1 * time.Minute

	MultipartMaxParts = 10000
	MultipartMinSize  = 5 * mib

	// Constants not exposed by aws-sdk-go
	// from /aws/signer/v4/internal/v4
	paramAmzDate       = "X-Amz-Date"
	paramAmzDateFormat = "20060102T150405Z"
)

var ErrClientEmpty = stderr.New("s3: storage client credentials not configured")

// SimpleStorageService - AWS S3 client.
// Data layer for file storage.
// Implements model.FileStorage interface
type SimpleStorageService struct {
	client        *s3.Client
	presignClient *s3.PresignClient
	bucket        string
	bufferSize    int
	contentType   *string
}

type StaticCredentials struct {
	Key    string `json:"key"`
	Secret string `json:"secret"`
	Token  string `json:"token"`
}

func (creds StaticCredentials) Validate() error {
	return validation.ValidateStruct(&creds,
		validation.Field(&creds.Key, validation.Required),
		validation.Field(&creds.Secret, validation.Required),
	)
}

func (creds StaticCredentials) awsCredentials() aws.Credentials {
	return aws.Credentials{
		AccessKeyID:     creds.Key,
		SecretAccessKey: creds.Secret,
		SessionToken:    creds.Token,
		Source:          "mender:StaticCredentials",
	}
}

func (creds StaticCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	return creds.awsCredentials(), nil
}

func newClient(
	ctx context.Context,
	withCredentials bool,
	opt *Options,
) (*SimpleStorageService, error) {
	if err := opt.Validate(); err != nil {
		return nil, errors.WithMessage(err, "s3: invalid configuration")
	}
	var (
		err error
		cfg aws.Config
	)

	if withCredentials {
		cfg, err = awsConfig.LoadDefaultConfig(ctx)
	} else {
		opt.StaticCredentials = nil
		cfg, err = awsConfig.LoadDefaultConfig(ctx,
			awsConfig.WithCredentialsProvider(aws.AnonymousCredentials{}),
		)
	}
	if err != nil {
		return nil, err
	}

	clientOpts, presignOpts := opt.toS3Options()
	client := s3.NewFromConfig(cfg, clientOpts)
	presignClient := s3.NewPresignClient(client, presignOpts)

	return &SimpleStorageService{
		client:        client,
		presignClient: presignClient,

		bufferSize:  *opt.BufferSize,
		contentType: opt.ContentType,
	}, nil
}

// NewEmpty initializes a new s3 client that does not implicitly load
// credentials from the environment. Credentials must be set using the
// StorageSettings provided with the Context.
func NewEmpty(ctx context.Context, opts ...*Options) (storage.ObjectStorage, error) {
	opt := NewOptions(opts...)
	return newClient(ctx, false, opt)
}

func New(ctx context.Context, bucket string, opts ...*Options) (storage.ObjectStorage, error) {
	opt := NewOptions(opts...)
	s3c, err := newClient(ctx, true, opt)
	if err != nil {
		return nil, err
	}
	s3c.bucket = bucket

	err = s3c.init(ctx)
	if err != nil {
		return nil, errors.WithMessage(err, "s3: failed to check bucket preconditions")
	}
	return s3c, nil
}

func (s *SimpleStorageService) init(ctx context.Context) error {
	hparams := &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	}
	var rspErr *awsHttp.ResponseError

	_, err := s.client.HeadBucket(ctx, hparams)
	if err == nil {
		// bucket exists and have permission to access it
		return nil
	} else if errors.As(err, &rspErr) {
		switch rspErr.Response.StatusCode {
		case http.StatusNotFound:
			err = nil // pass
		case http.StatusForbidden:
			err = fmt.Errorf(
				"s3: insufficient permissions for accessing bucket '%s'",
				s.bucket,
			)
		}
	}
	if err != nil {
		return err
	}
	cparams := &s3.CreateBucketInput{
		Bucket: aws.String(s.bucket),
	}

	_, err = s.client.CreateBucket(ctx, cparams)
	if err != nil {
		var errBucket *types.BucketAlreadyOwnedByYou
		if !errors.As(err, errBucket) {
			return errors.WithMessage(err, "s3: error creating bucket")
		}
	}
	waitTime := time.Second * 30
	if deadline, ok := ctx.Deadline(); ok {
		waitTime = time.Until(deadline)
	}
	err = s3.NewBucketExistsWaiter(s.client).
		Wait(ctx, &s3.HeadBucketInput{Bucket: &s.bucket}, waitTime)
	return err
}

func noOpts(*s3.Options) {
}

func (s *SimpleStorageService) optionsFromContext(
	ctx context.Context,
	presign bool,
) (bucket string, clientOptions func(*s3.Options), err error) {
	if settings := settingsFromContext(ctx); settings != nil {
		bucket = settings.Bucket
		clientOptions, err = settings.getOptions(presign)
	} else if s.bucket == "" {
		return "", nil, ErrClientEmpty
	} else {
		bucket = s.bucket
		clientOptions = noOpts
	}
	return bucket, clientOptions, err
}

func (s *SimpleStorageService) HealthCheck(ctx context.Context) error {
	bucket, opts, err := s.optionsFromContext(ctx, false)
	if err != nil {
		return err
	}
	_, err = s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	}, opts)
	return err
}

type objectReader struct {
	io.ReadCloser
	length int64
}

func (obj objectReader) Length() int64 {
	return obj.length
}

func (s *SimpleStorageService) GetObject(
	ctx context.Context,
	path string,
) (io.ReadCloser, error) {
	bucket, opts, err := s.optionsFromContext(ctx, false)
	if err != nil {
		return nil, err
	}
	params := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),

		RequestPayer: types.RequestPayerRequester,
	}

	out, err := s.client.GetObject(ctx, params, opts)
	var rspErr *awsHttp.ResponseError
	if errors.As(err, &rspErr) {
		if rspErr.Response.StatusCode == http.StatusNotFound {
			err = storage.ErrObjectNotFound
		}
	}
	if err != nil {
		return nil, errors.WithMessage(
			err,
			"s3: failed to get object",
		)
	}
	return objectReader{
		ReadCloser: out.Body,
		length:     out.ContentLength,
	}, nil
}

// Delete removes deleted file from storage.
// Noop if ID does not exist.
func (s *SimpleStorageService) DeleteObject(ctx context.Context, path string) error {
	bucket, opts, err := s.optionsFromContext(ctx, false)
	if err != nil {
		return err
	}

	params := &s3.DeleteObjectInput{
		// Required
		Bucket: aws.String(bucket),
		Key:    aws.String(path),

		// Optional
		RequestPayer: types.RequestPayerRequester,
	}

	// ignore return response which contains charing info
	// and file versioning data which are not in interest
	_, err = s.client.DeleteObject(ctx, params, opts)
	var rspErr *awsHttp.ResponseError
	if errors.As(err, &rspErr) {
		if rspErr.Response.StatusCode == http.StatusNotFound {
			err = storage.ErrObjectNotFound
		}
	}
	if err != nil {
		return errors.WithMessage(err, "s3: error deleting object")
	}

	return nil
}

// Exists check if selected object exists in the storage
func (s *SimpleStorageService) StatObject(
	ctx context.Context,
	path string,
) (*storage.ObjectInfo, error) {

	bucket, opts, err := s.optionsFromContext(ctx, false)
	if err != nil {
		return nil, err
	}

	params := &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	}
	rsp, err := s.client.HeadObject(ctx, params, opts)
	var rspErr *awsHttp.ResponseError
	if errors.As(err, &rspErr) {
		if rspErr.Response.StatusCode == http.StatusNotFound {
			err = storage.ErrObjectNotFound
		}
	}
	if err != nil {
		return nil, errors.WithMessage(err, "s3: error getting object info")
	}

	return &storage.ObjectInfo{
		Path:         path,
		LastModified: rsp.LastModified,
		Size:         &rsp.ContentLength,
	}, nil
}

func fillBuffer(b []byte, r io.Reader) (int, error) {
	var offset int
	var err error
	for n := 0; offset < len(b) && err == nil; offset += n {
		n, err = r.Read(b[offset:])
	}
	return offset, err
}

// uploadMultipart uploads an artifact using the multipart API.
func (s *SimpleStorageService) uploadMultipart(
	ctx context.Context,
	buf []byte,
	objectPath string,
	artifact io.Reader,
) error {
	const maxPartNum = 10000
	var partNum int32 = 1
	var rspUpload *s3.UploadPartOutput
	bucket, opts, err := s.optionsFromContext(ctx, true)
	if err != nil {
		return err
	}

	// Pre-allocate 100 completed part (generous guesstimate)
	completedParts := make([]types.CompletedPart, 0, 100)

	// Initiate Multipart upload
	createParams := &s3.CreateMultipartUploadInput{
		Bucket:      &bucket,
		Key:         &objectPath,
		ContentType: s.contentType,
	}
	rspCreate, err := s.client.CreateMultipartUpload(
		ctx, createParams, opts,
	)
	if err != nil {
		return err
	}
	uploadParams := &s3.UploadPartInput{
		Bucket:     &bucket,
		Key:        &objectPath,
		UploadId:   rspCreate.UploadId,
		PartNumber: partNum,
	}

	// Upload the first chunk already stored in buffer
	r := bytes.NewReader(buf)
	// Readjust upload parameters
	uploadParams.Body = r
	rspUpload, err = s.client.UploadPart(
		ctx,
		uploadParams,
		opts,
	)
	if err != nil {
		return err
	}
	completedParts = append(
		completedParts,
		types.CompletedPart{
			ETag:       rspUpload.ETag,
			PartNumber: partNum,
		},
	)

	// The following is loop is very similar to io.Copy except the
	// destination is the s3 bucket.
	for partNum++; partNum < maxPartNum; partNum++ {
		// Read next chunk from stream (fill the whole buffer)
		offset, eRead := fillBuffer(buf, artifact)
		if offset > 0 {
			r := bytes.NewReader(buf[:offset])
			// Readjust upload parameters
			uploadParams.PartNumber = partNum
			uploadParams.Body = r
			rspUpload, err = s.client.UploadPart(
				ctx,
				uploadParams,
				opts,
			)
			if err != nil {
				break
			}
			completedParts = append(
				completedParts,
				types.CompletedPart{
					ETag:       rspUpload.ETag,
					PartNumber: partNum,
				},
			)
		} else {
			// Read did not return any bytes
			break
		}
		if eRead != nil {
			err = eRead
			break
		}
	}
	if err == nil || err == io.EOF {
		// Complete upload
		uploadParams := &s3.CompleteMultipartUploadInput{
			Bucket:   &bucket,
			Key:      &objectPath,
			UploadId: rspCreate.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{
				Parts: completedParts,
			},
		}
		_, err = s.client.CompleteMultipartUpload(
			ctx,
			uploadParams,
			opts,
		)
	} else {
		// Abort multipart upload!
		uploadParams := &s3.AbortMultipartUploadInput{
			Bucket:   &bucket,
			Key:      &objectPath,
			UploadId: rspCreate.UploadId,
		}
		_, _ = s.client.AbortMultipartUpload(
			ctx,
			uploadParams,
			opts,
		)
	}
	return err
}

// UploadArtifact uploads given artifact into the file server (AWS S3 or minio)
// using objectID as a key. If the artifact is larger than 5 MiB, the file is
// uploaded using the s3 multipart API, otherwise the object is created in a
// single request.
func (s *SimpleStorageService) PutObject(
	ctx context.Context,
	path string,
	src io.Reader,
) error {
	var (
		r   io.Reader
		l   int64
		n   int
		err error
		buf []byte
	)
	if objReader, ok := src.(storage.ObjectReader); ok {
		r = objReader
		l = objReader.Length()
	} else {
		// Peek payload
		buf = make([]byte, s.bufferSize)
		n, err = fillBuffer(buf, src)
		if err == io.EOF {
			r = bytes.NewReader(buf[:n])
			l = int64(n)
		}
	}

	// If only one part, use PutObject API.
	if r != nil {
		var (
			bucket string
			opts   func(*s3.Options)
		)
		bucket, opts, err = s.optionsFromContext(ctx, true)
		if err != nil {
			return err
		}
		// Ordinary single-file upload
		uploadParams := &s3.PutObjectInput{
			Body:          r,
			Bucket:        &bucket,
			Key:           &path,
			ContentType:   s.contentType,
			ContentLength: l,
		}
		_, err = s.client.PutObject(
			ctx,
			uploadParams,
			opts,
		)
	} else if err == nil {
		err = s.uploadMultipart(ctx, buf, path, src)
	}
	return err
}

func (s *SimpleStorageService) PutRequest(
	ctx context.Context,
	path string,
	expireAfter time.Duration,
) (*model.Link, error) {

	expireAfter = capDurationToLimits(expireAfter).Truncate(time.Second)
	bucket, opts, err := s.optionsFromContext(ctx, true)
	if err != nil {
		return nil, err
	}

	params := &s3.PutObjectInput{
		// Required
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	}

	signDate := time.Now()
	req, err := s.presignClient.PresignPutObject(
		ctx,
		params,
		s3.WithPresignExpires(expireAfter),
		s3.WithPresignClientFromClientOptions(opts),
	)
	if err != nil {
		return nil, err
	}
	if date, err := time.Parse(
		req.SignedHeader.Get(paramAmzDate), paramAmzDateFormat,
	); err == nil {
		signDate = date
	}

	return &model.Link{
		Uri:    req.URL,
		Expire: signDate.Add(expireAfter),
		Method: http.MethodPut,
	}, nil
}

// GetRequest duration is limited to 7 days (AWS limitation)
func (s *SimpleStorageService) GetRequest(
	ctx context.Context,
	objectPath string,
	filename string,
	expireAfter time.Duration,
) (*model.Link, error) {

	expireAfter = capDurationToLimits(expireAfter).Truncate(time.Second)
	bucket, opts, err := s.optionsFromContext(ctx, true)
	if err != nil {
		return nil, err
	}

	if _, err := s.StatObject(ctx, objectPath); err != nil {
		return nil, errors.WithMessage(err, "s3: head object")
	}

	params := &s3.GetObjectInput{
		Bucket:              aws.String(bucket),
		Key:                 aws.String(objectPath),
		ResponseContentType: s.contentType,
	}

	if filename != "" {
		contentDisposition := fmt.Sprintf("attachment; filename=\"%s\"", filename)
		params.ResponseContentDisposition = &contentDisposition
	}

	signDate := time.Now()
	req, err := s.presignClient.PresignGetObject(ctx,
		params,
		s3.WithPresignExpires(expireAfter),
		s3.WithPresignClientFromClientOptions(opts))
	if err != nil {
		return nil, errors.WithMessage(err, "s3: failed to sign GET request")
	}
	if date, err := time.Parse(
		req.SignedHeader.Get(paramAmzDate), paramAmzDateFormat,
	); err == nil {
		signDate = date
	}

	return &model.Link{
		Uri:    req.URL,
		Expire: signDate.Add(expireAfter),
		Method: http.MethodGet,
	}, nil
}

// DeleteRequest returns a presigned deletion request
func (s *SimpleStorageService) DeleteRequest(
	ctx context.Context,
	path string,
	expireAfter time.Duration,
) (*model.Link, error) {

	expireAfter = capDurationToLimits(expireAfter).Truncate(time.Second)
	bucket, opts, err := s.optionsFromContext(ctx, true)
	if err != nil {
		return nil, err
	}

	params := &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	}

	signDate := time.Now()
	req, err := s.presignClient.PresignDeleteObject(ctx,
		params,
		s3.WithPresignExpires(expireAfter),
		s3.WithPresignClientFromClientOptions(opts))
	if err != nil {
		return nil, errors.WithMessage(err, "s3: failed to sign DELETE request")
	}
	if date, err := time.Parse(
		req.SignedHeader.Get(paramAmzDate), paramAmzDateFormat,
	); err == nil {
		signDate = date
	}

	return &model.Link{
		Uri:    req.URL,
		Expire: signDate.Add(expireAfter),
		Method: http.MethodDelete,
	}, nil
}

// presign requests are limited to 7 days
func capDurationToLimits(duration time.Duration) time.Duration {
	if duration < ExpireMinLimit {
		duration = ExpireMinLimit
	} else if duration > ExpireMaxLimit {
		duration = ExpireMaxLimit
	}
	return duration
}
