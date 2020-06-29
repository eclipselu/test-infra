/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

type Client struct {
	*storage.Client
}

// NewClient creates new GCS client with given service account
func NewClient(ctx context.Context, serviceAccount string) (*Client, error) {
	client, err := storage.NewClient(ctx, option.WithCredentialsFile(serviceAccount))
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}

// NewStorageBucket creates a new bucket in GCS with uniform access policy
func (g *Client) NewStorageBucket(ctx context.Context, bucketName, project string) error {
	if project == "" {
		return errors.New("a project must be provided")
	}

	if bucketName == "" {
		return errors.New("a bucket name must be provided")
	}

	bucket := g.Bucket(bucketName)

	// For now, this creates a bucket with uniform policy across its objects to make ACL
	// and permission management simple instead of object-level granularity that we currently
	// do not use anyway.
	bucketAttr := &storage.BucketAttrs{
		BucketPolicyOnly: storage.BucketPolicyOnly{
			Enabled: true,
		},
	}

	return bucket.Create(ctx, project, bucketAttr)
}

// DeleteStorageBucket removes all children objects and then deletes the bucket
func (g *Client) DeleteStorageBucket(ctx context.Context, bucketName string, force bool) error {
	children, err := g.ListChildrenFiles(ctx, bucketName, "")
	if err != nil {
		return err
	}

	if len(children) == 0 && !force {
		return fmt.Errorf("bucket %s not empty, please use force=true", bucketName)
	}

	for _, child := range children {
		if err := g.DeleteObject(ctx, bucketName, child); err != nil {
			return err
		}
	}
	return g.Bucket(bucketName).Delete(ctx)
}

// get objects iterator under given storagePath and bucketName, use exclusionFilter to eliminate some files.
func (g *Client) getObjectsIter(ctx context.Context, bucketName, storagePath, exclusionFilter string) *storage.ObjectIterator {
	return g.Bucket(bucketName).Objects(ctx, &storage.Query{
		Prefix:    storagePath,
		Delimiter: exclusionFilter,
	})
}

// Exists check if an object exists under a bucket, assuming bucket exists
func (g *Client) Exists(ctx context.Context, bucketName, objPath string) bool {
	// Check if this is a file
	objHandle := g.Bucket(bucketName).Object(objPath)
	if _, err := objHandle.Attrs(ctx); err == nil {
		return true
	}

	// Check if this is a directory,
	// gcs directory paths are virtual paths, they automatically get deleted if there is no child file
	_, err := g.getObjectsIter(ctx, bucketName, strings.TrimRight(objPath, " /")+"/", "").Next()
	return err == nil
}

// list child under storagePath, use exclusionFilter for skipping some files.
// This function gets all child files recursively under given storagePath,
// then filter out filenames containing given exclusionFilter.
// If exclusionFilter is empty string, returns all files but not directories,
// if exclusionFilter is "/", returns all direct children, including both files and directories.
// see https://godoc.org/cloud.google.com/go/storage#Query
func (g *Client) list(ctx context.Context, bucketName, storagePath, exclusionFilter string) ([]string, error) {
	objsAttrs, err := g.getObjectsAttrs(ctx, bucketName, storagePath, exclusionFilter)
	if err != nil {
		return nil, err
	}
	filePaths := make([]string, 0, len(objsAttrs))
	for _, attrs := range objsAttrs {
		filePaths = append(filePaths, path.Join(attrs.Prefix, attrs.Name))
	}
	return filePaths, nil
}

// Query items under given gcs storagePath, use exclusionFilter to eliminate some files.
func (g *Client) getObjectsAttrs(ctx context.Context, bucketName, storagePath,
	exclusionFilter string) ([]*storage.ObjectAttrs, error) {
	var allAttrs []*storage.ObjectAttrs
	it := g.getObjectsIter(ctx, bucketName, storagePath, exclusionFilter)

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error iterating: %v", err)
		}
		allAttrs = append(allAttrs, attrs)
	}
	return allAttrs, nil
}

func (g *Client) listChildren(ctx context.Context, bucketName, dirPath, exclusionFilter string) ([]string, error) {
	if dirPath != "" {
		dirPath = strings.TrimRight(dirPath, " /") + "/"
	}

	return g.list(ctx, bucketName, dirPath, exclusionFilter)
}

// ListChildrenFiles recursively lists all children files.
func (g *Client) ListChildrenFiles(ctx context.Context, bucketName, dirPath string) ([]string, error) {
	return g.listChildren(ctx, bucketName, dirPath, "")
}

// ListDirectChildren lists direct children paths (including files and directories).
func (g *Client) ListDirectChildren(ctx context.Context, bucketName, dirPath string) ([]string, error) {
	// If there are 2 directories named "foo" and "foobar",
	// then given storagePath "foo" will get files both under "foo" and "foobar".
	// Add trailling slash to storagePath, so that only gets children under given directory.
	return g.listChildren(ctx, bucketName, dirPath, "/")
}

// CopyObject copies objects from one location to another. Assumes both source and destination buckets exist.
func (g *Client) CopyObject(ctx context.Context, srcBucketName, srcPath, dstBucketName, dstPath string) error {
	src := g.Bucket(srcBucketName).Object(srcPath)
	dst := g.Bucket(dstBucketName).Object(dstPath)

	_, err := dst.CopierFrom(src).Run(ctx)
	return err
}

// Download gcs object to a file
func (g *Client) Download(ctx context.Context, bucketName, objPath, dstPath string) error {
	handle := g.Bucket(bucketName).Object(objPath)
	if _, err := handle.Attrs(ctx); err != nil {
		return err
	}

	dst, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	src, err := handle.NewReader(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(dst, src)
	return err
}

// Upload file to gcs object
func (g *Client) Upload(ctx context.Context, bucketName, objPath, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	dst := g.Bucket(bucketName).Object(objPath).NewWriter(ctx)
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}

// AttrObject returns the object attributes
func (g *Client) AttrObject(ctx context.Context, bucketName, objPath string) (*storage.ObjectAttrs, error) {
	objHandle := g.Bucket(bucketName).Object(objPath)
	return objHandle.Attrs(ctx)
}

// ReadObject reads the content of a gcs object
func (g *Client) ReadObject(ctx context.Context, bucketName, objPath string) ([]byte, error) {
	var contents []byte
	f, err := g.NewReader(ctx, bucketName, objPath)
	if err != nil {
		return contents, err
	}
	defer f.Close()
	return ioutil.ReadAll(f)
}

// NewReader creates a new Reader of a gcs file.
// Important: caller must call Close on the returned Reader when done reading
func (g *Client) NewReader(ctx context.Context, bucketName, objPath string) (*storage.Reader, error) {
	o := g.Bucket(bucketName).Object(objPath)
	if _, err := o.Attrs(ctx); err != nil {
		return nil, err
	}
	return o.NewReader(ctx)
}

// DeleteObject deletes an object
func (g *Client) DeleteObject(ctx context.Context, bucketName, objPath string) error {
	objHandle := g.Bucket(bucketName).Object(objPath)
	return objHandle.Delete(ctx)
}

// WriteObject writes the content to a gcs object
func (g *Client) WriteObject(ctx context.Context, bucketName, objPath string,
	content []byte) (n int, err error) {
	objWriter := g.Bucket(bucketName).Object(objPath).NewWriter(ctx)
	defer func() {
		cerr := objWriter.Close()
		if err == nil {
			err = cerr
		}
	}()

	n, err = objWriter.Write(content)
	return
}

// ReadURL reads from a gsUrl and return a log structure
func (g *Client) ReadURL(ctx context.Context, gcsURL string) ([]byte, error) {
	bucket, obj, err := linkToBucketAndObject(gcsURL)
	if err != nil {
		return nil, err
	}

	return g.ReadObject(ctx, bucket, obj)
}