/*
 * Minimalist Object Storage, (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package donut

import (
	"bytes"
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio/pkg/iodine"
)

// NewMultipartUpload -
func (donut API) NewMultipartUpload(bucket, key, contentType string) (string, error) {
	donut.lock.Lock()
	defer donut.lock.Unlock()
	if !IsValidBucket(bucket) {
		return "", iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if !IsValidObjectName(key) {
		return "", iodine.New(ObjectNameInvalid{Object: key}, nil)
	}
	if _, ok := donut.storedBuckets[bucket]; ok == false {
		return "", iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	storedBucket := donut.storedBuckets[bucket]
	objectKey := bucket + "/" + key
	if _, ok := storedBucket.objectMetadata[objectKey]; ok == true {
		return "", iodine.New(ObjectExists{Object: key}, nil)
	}
	id := []byte(strconv.FormatInt(rand.Int63(), 10) + bucket + key + time.Now().String())
	uploadIDSum := sha512.Sum512(id)
	uploadID := base64.URLEncoding.EncodeToString(uploadIDSum[:])[:47]

	donut.storedBuckets[bucket].multiPartSession[key] = multiPartSession{
		uploadID:   uploadID,
		initiated:  time.Now(),
		totalParts: 0,
	}

	return uploadID, nil
}

// AbortMultipartUpload -
func (donut API) AbortMultipartUpload(bucket, key, uploadID string) error {
	donut.lock.Lock()
	defer donut.lock.Unlock()
	storedBucket := donut.storedBuckets[bucket]
	if storedBucket.multiPartSession[key].uploadID != uploadID {
		return iodine.New(InvalidUploadID{UploadID: uploadID}, nil)
	}
	donut.cleanupMultiparts(bucket, key, uploadID)
	donut.cleanupMultipartSession(bucket, key, uploadID)
	return nil
}

func getMultipartKey(key string, uploadID string, partNumber int) string {
	return key + "?uploadId=" + uploadID + "&partNumber=" + strconv.Itoa(partNumber)
}

// CreateObjectPart -
func (donut API) CreateObjectPart(bucket, key, uploadID string, partID int, contentType, expectedMD5Sum string, size int64, data io.Reader) (string, error) {
	// Verify upload id
	storedBucket := donut.storedBuckets[bucket]
	if storedBucket.multiPartSession[key].uploadID != uploadID {
		return "", iodine.New(InvalidUploadID{UploadID: uploadID}, nil)
	}

	donut.lock.Lock()
	etag, err := donut.createObjectPart(bucket, key, uploadID, partID, "", expectedMD5Sum, size, data)
	if err != nil {
		return "", iodine.New(err, nil)
	}
	donut.lock.Unlock()

	// free
	debug.FreeOSMemory()
	return etag, nil
}

// createObject - PUT object to cache buffer
func (donut API) createObjectPart(bucket, key, uploadID string, partID int, contentType, expectedMD5Sum string, size int64, data io.Reader) (string, error) {
	if !IsValidBucket(bucket) {
		return "", iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if !IsValidObjectName(key) {
		return "", iodine.New(ObjectNameInvalid{Object: key}, nil)
	}
	if _, ok := donut.storedBuckets[bucket]; ok == false {
		return "", iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	storedBucket := donut.storedBuckets[bucket]
	// get object key
	partKey := bucket + "/" + getMultipartKey(key, uploadID, partID)
	if _, ok := storedBucket.partMetadata[partKey]; ok == true {
		return storedBucket.partMetadata[partKey].ETag, nil
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentType = strings.TrimSpace(contentType)
	if strings.TrimSpace(expectedMD5Sum) != "" {
		expectedMD5SumBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(expectedMD5Sum))
		if err != nil {
			// pro-actively close the connection
			return "", iodine.New(InvalidDigest{Md5: expectedMD5Sum}, nil)
		}
		expectedMD5Sum = hex.EncodeToString(expectedMD5SumBytes)
	}

	// calculate md5
	hash := md5.New()
	var readBytes []byte

	var err error
	var length int
	for err == nil {
		byteBuffer := make([]byte, 1024*1024)
		length, err = data.Read(byteBuffer)
		// While hash.Write() wouldn't mind a Nil byteBuffer
		// It is necessary for us to verify this and break
		if length == 0 {
			break
		}
		hash.Write(byteBuffer[0:length])
		readBytes = append(readBytes, byteBuffer[0:length]...)
	}
	if err != io.EOF {
		return "", iodine.New(err, nil)
	}
	go debug.FreeOSMemory()
	md5SumBytes := hash.Sum(nil)
	totalLength := int64(len(readBytes))

	donut.multiPartObjects.Set(partKey, readBytes)
	// setting up for de-allocation
	readBytes = nil

	md5Sum := hex.EncodeToString(md5SumBytes)
	// Verify if the written object is equal to what is expected, only if it is requested as such
	if strings.TrimSpace(expectedMD5Sum) != "" {
		if err := isMD5SumEqual(strings.TrimSpace(expectedMD5Sum), md5Sum); err != nil {
			return "", iodine.New(BadDigest{}, nil)
		}
	}
	newPart := PartMetadata{
		PartNumber:   partID,
		LastModified: time.Now().UTC(),
		ETag:         md5Sum,
		Size:         totalLength,
	}

	storedBucket.partMetadata[partKey] = newPart
	multiPartSession := storedBucket.multiPartSession[key]
	multiPartSession.totalParts++
	storedBucket.multiPartSession[key] = multiPartSession
	donut.storedBuckets[bucket] = storedBucket

	return md5Sum, nil
}

func (donut API) cleanupMultipartSession(bucket, key, uploadID string) {
	delete(donut.storedBuckets[bucket].multiPartSession, key)
}

func (donut API) cleanupMultiparts(bucket, key, uploadID string) {
	for i := 1; i <= donut.storedBuckets[bucket].multiPartSession[key].totalParts; i++ {
		objectKey := bucket + "/" + getMultipartKey(key, uploadID, i)
		donut.multiPartObjects.Delete(objectKey)
	}
}

// CompleteMultipartUpload -
func (donut API) CompleteMultipartUpload(bucket, key, uploadID string, parts map[int]string) (ObjectMetadata, error) {
	donut.lock.Lock()
	if !IsValidBucket(bucket) {
		return ObjectMetadata{}, iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if !IsValidObjectName(key) {
		return ObjectMetadata{}, iodine.New(ObjectNameInvalid{Object: key}, nil)
	}
	// Verify upload id
	if _, ok := donut.storedBuckets[bucket]; ok == false {
		return ObjectMetadata{}, iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	storedBucket := donut.storedBuckets[bucket]
	if storedBucket.multiPartSession[key].uploadID != uploadID {
		return ObjectMetadata{}, iodine.New(InvalidUploadID{UploadID: uploadID}, nil)
	}
	var size int64
	var fullObject bytes.Buffer
	for i := 1; i <= len(parts); i++ {
		recvMD5 := parts[i]
		object, ok := donut.multiPartObjects.Get(bucket + "/" + getMultipartKey(key, uploadID, i))
		if ok == false {
			donut.lock.Unlock()
			return ObjectMetadata{}, iodine.New(errors.New("missing part: "+strconv.Itoa(i)), nil)
		}
		size += int64(len(object))
		calcMD5Bytes := md5.Sum(object)
		// complete multi part request header md5sum per part is hex encoded
		recvMD5Bytes, err := hex.DecodeString(strings.Trim(recvMD5, "\""))
		if err != nil {
			return ObjectMetadata{}, iodine.New(InvalidDigest{Md5: recvMD5}, nil)
		}
		if !bytes.Equal(recvMD5Bytes, calcMD5Bytes[:]) {
			return ObjectMetadata{}, iodine.New(BadDigest{}, nil)
		}
		_, err = io.Copy(&fullObject, bytes.NewBuffer(object))
		if err != nil {
			return ObjectMetadata{}, iodine.New(err, nil)
		}
		object = nil
		go debug.FreeOSMemory()
	}

	md5sumSlice := md5.Sum(fullObject.Bytes())
	// this is needed for final verification inside CreateObject, do not convert this to hex
	md5sum := base64.StdEncoding.EncodeToString(md5sumSlice[:])
	donut.lock.Unlock()
	objectMetadata, err := donut.CreateObject(bucket, key, md5sum, size, &fullObject, nil)
	if err != nil {
		// No need to call internal cleanup functions here, caller will call AbortMultipartUpload()
		// which would in-turn cleanup properly in accordance with S3 Spec
		return ObjectMetadata{}, iodine.New(err, nil)
	}
	fullObject.Reset()
	donut.lock.Lock()
	donut.cleanupMultiparts(bucket, key, uploadID)
	donut.cleanupMultipartSession(bucket, key, uploadID)
	donut.lock.Unlock()
	return objectMetadata, nil
}

// byKey is a sortable interface for UploadMetadata slice
type byKey []*UploadMetadata

func (a byKey) Len() int           { return len(a) }
func (a byKey) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byKey) Less(i, j int) bool { return a[i].Key < a[j].Key }

// ListMultipartUploads -
func (donut API) ListMultipartUploads(bucket string, resources BucketMultipartResourcesMetadata) (BucketMultipartResourcesMetadata, error) {
	// TODO handle delimiter
	donut.lock.Lock()
	defer donut.lock.Unlock()
	if _, ok := donut.storedBuckets[bucket]; ok == false {
		return BucketMultipartResourcesMetadata{}, iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	storedBucket := donut.storedBuckets[bucket]
	var uploads []*UploadMetadata

	for key, session := range storedBucket.multiPartSession {
		if strings.HasPrefix(key, resources.Prefix) {
			if len(uploads) > resources.MaxUploads {
				sort.Sort(byKey(uploads))
				resources.Upload = uploads
				resources.NextKeyMarker = key
				resources.NextUploadIDMarker = session.uploadID
				resources.IsTruncated = true
				return resources, nil
			}
			// uploadIDMarker is ignored if KeyMarker is empty
			switch {
			case resources.KeyMarker != "" && resources.UploadIDMarker == "":
				if key > resources.KeyMarker {
					upload := new(UploadMetadata)
					upload.Key = key
					upload.UploadID = session.uploadID
					upload.Initiated = session.initiated
					uploads = append(uploads, upload)
				}
			case resources.KeyMarker != "" && resources.UploadIDMarker != "":
				if session.uploadID > resources.UploadIDMarker {
					if key >= resources.KeyMarker {
						upload := new(UploadMetadata)
						upload.Key = key
						upload.UploadID = session.uploadID
						upload.Initiated = session.initiated
						uploads = append(uploads, upload)
					}
				}
			default:
				upload := new(UploadMetadata)
				upload.Key = key
				upload.UploadID = session.uploadID
				upload.Initiated = session.initiated
				uploads = append(uploads, upload)
			}
		}
	}
	sort.Sort(byKey(uploads))
	resources.Upload = uploads
	return resources, nil
}

// partNumber is a sortable interface for Part slice
type partNumber []*PartMetadata

func (a partNumber) Len() int           { return len(a) }
func (a partNumber) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a partNumber) Less(i, j int) bool { return a[i].PartNumber < a[j].PartNumber }

// ListObjectParts -
func (donut API) ListObjectParts(bucket, key string, resources ObjectResourcesMetadata) (ObjectResourcesMetadata, error) {
	// Verify upload id
	donut.lock.Lock()
	defer donut.lock.Unlock()
	if _, ok := donut.storedBuckets[bucket]; ok == false {
		return ObjectResourcesMetadata{}, iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	storedBucket := donut.storedBuckets[bucket]
	if _, ok := storedBucket.multiPartSession[key]; ok == false {
		return ObjectResourcesMetadata{}, iodine.New(ObjectNotFound{Object: key}, nil)
	}
	if storedBucket.multiPartSession[key].uploadID != resources.UploadID {
		return ObjectResourcesMetadata{}, iodine.New(InvalidUploadID{UploadID: resources.UploadID}, nil)
	}
	objectResourcesMetadata := resources
	objectResourcesMetadata.Bucket = bucket
	objectResourcesMetadata.Key = key
	var parts []*PartMetadata
	var startPartNumber int
	switch {
	case objectResourcesMetadata.PartNumberMarker == 0:
		startPartNumber = 1
	default:
		startPartNumber = objectResourcesMetadata.PartNumberMarker
	}
	for i := startPartNumber; i <= storedBucket.multiPartSession[key].totalParts; i++ {
		if len(parts) > objectResourcesMetadata.MaxParts {
			sort.Sort(partNumber(parts))
			objectResourcesMetadata.IsTruncated = true
			objectResourcesMetadata.Part = parts
			objectResourcesMetadata.NextPartNumberMarker = i
			return objectResourcesMetadata, nil
		}
		part, ok := storedBucket.partMetadata[bucket+"/"+getMultipartKey(key, resources.UploadID, i)]
		if !ok {
			return ObjectResourcesMetadata{}, iodine.New(errors.New("missing part: "+strconv.Itoa(i)), nil)
		}
		parts = append(parts, &part)
	}
	sort.Sort(partNumber(parts))
	objectResourcesMetadata.Part = parts
	return objectResourcesMetadata, nil
}

func (donut API) expiredPart(a ...interface{}) {
	key := a[0].(string)
	// loop through all buckets
	for _, storedBucket := range donut.storedBuckets {
		delete(storedBucket.partMetadata, key)
	}
	debug.FreeOSMemory()
}