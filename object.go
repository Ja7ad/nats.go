// Copyright 2021-2022 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nats

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nuid"
)

// ObjectStoreManager creates, loads and deletes Object Stores
//
// Notice: Experimental Preview
//
// This functionality is EXPERIMENTAL and may be changed in later releases.
type ObjectStoreManager interface {
	// ObjectStore will look up and bind to an existing object store instance.
	ObjectStore(bucket string) (ObjectStore, error)
	// CreateObjectStore will create an object store.
	CreateObjectStore(cfg *ObjectStoreConfig) (ObjectStore, error)
	// DeleteObjectStore will delete the underlying stream for the named object.
	DeleteObjectStore(bucket string) error
}

// ObjectStore is a blob store capable of storing large objects efficiently in
// JetStream streams
//
// Notice: Experimental Preview
//
// This functionality is EXPERIMENTAL and may be changed in later releases.
type ObjectStore interface {
	// Put will place the contents from the reader into a new object.
	Put(obj *ObjectMeta, reader io.Reader, opts ...ObjectOpt) (*ObjectInfo, error)
	// Get will pull the named object from the object store.
	Get(name string, opts ...ObjectOpt) (ObjectResult, error)

	// PutBytes is convenience function to put a byte slice into this object store.
	PutBytes(name string, data []byte, opts ...ObjectOpt) (*ObjectInfo, error)
	// GetBytes is a convenience function to pull an object from this object store and return it as a byte slice.
	GetBytes(name string, opts ...ObjectOpt) ([]byte, error)

	// PutString is convenience function to put a string into this object store.
	PutString(name string, data string, opts ...ObjectOpt) (*ObjectInfo, error)
	// GetString is a convenience function to pull an object from this object store and return it as a string.
	GetString(name string, opts ...ObjectOpt) (string, error)

	// PutFile is convenience function to put a file into this object store.
	PutFile(file string, opts ...ObjectOpt) (*ObjectInfo, error)
	// GetFile is a convenience function to pull an object from this object store and place it in a file.
	GetFile(name, file string, opts ...ObjectOpt) error

	// GetInfo will retrieve the current information for the object.
	GetInfo(name string) (*ObjectInfo, error)
	// UpdateMeta will update the meta data for the object.
	UpdateMeta(name string, meta *ObjectMeta) error

	// Delete will delete the named object.
	Delete(name string) error

	// AddLink will add a link to another object.
	AddLink(name string, obj *ObjectInfo) (*ObjectInfo, error)

	// AddBucketLink will add a link to another object store.
	AddBucketLink(name string, bucket ObjectStore) (*ObjectInfo, error)

	// Seal will seal the object store, no further modifications will be allowed.
	Seal() error

	// Watch for changes in the underlying store and receive meta information updates.
	Watch(opts ...WatchOpt) (ObjectWatcher, error)

	// List will list all the objects in this store.
	List(opts ...WatchOpt) ([]*ObjectInfo, error)

	// Status retrieves run-time status about the backing store of the bucket.
	Status() (ObjectStoreStatus, error)
}

type ObjectOpt interface {
	configureObject(opts *objOpts) error
}

type objOpts struct {
	ctx context.Context
}

// For nats.Context() support.
func (ctx ContextOpt) configureObject(opts *objOpts) error {
	opts.ctx = ctx
	return nil
}

// ObjectWatcher is what is returned when doing a watch.
type ObjectWatcher interface {
	// Updates returns a channel to read any updates to entries.
	Updates() <-chan *ObjectInfo
	// Stop will stop this watcher.
	Stop() error
}

var (
	ErrObjectConfigRequired = errors.New("nats: object-store config required")
	ErrBadObjectMeta        = errors.New("nats: object-store meta information invalid")
	ErrObjectNotFound       = errors.New("nats: object not found")
	ErrInvalidStoreName     = errors.New("nats: invalid object-store name")
	ErrDigestMismatch       = errors.New("nats: received a corrupt object, digests do not match")
	ErrInvalidDigestFormat  = errors.New("nats: object digest hash has invalid format")
	ErrNoObjectsFound       = errors.New("nats: no objects found")
	ErrObjectAlreadyExists  = errors.New("nats: an object already exists with that name")
	ErrNameRequired         = errors.New("nats: name is required")
	ErrNeeds262             = errors.New("nats: object-store requires at least server version 2.6.2")
)

// ObjectStoreConfig is the config for the object store.
type ObjectStoreConfig struct {
	Bucket      string
	Description string
	TTL         time.Duration
	MaxBytes    int64
	Storage     StorageType
	Replicas    int
	Placement   *Placement
}

type ObjectStoreStatus interface {
	// Bucket is the name of the bucket
	Bucket() string
	// Description is the description supplied when creating the bucket
	Description() string
	// TTL indicates how long objects are kept in the bucket
	TTL() time.Duration
	// Storage indicates the underlying JetStream storage technology used to store data
	Storage() StorageType
	// Replicas indicates how many storage replicas are kept for the data in the bucket
	Replicas() int
	// Sealed indicates the stream is sealed and cannot be modified in any way
	Sealed() bool
	// Size is the combined size of all data in the bucket including metadata, in bytes
	Size() uint64
	// BackingStore provides details about the underlying storage
	BackingStore() string
}

// ObjectMetaOptions
type ObjectMetaOptions struct {
	Link      *ObjectLink `json:"link,omitempty"`
	ChunkSize uint32      `json:"max_chunk_size,omitempty"`
}

// ObjectMeta is high level information about an object.
type ObjectMeta struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Headers     Header `json:"headers,omitempty"`

	// Optional options.
	Opts *ObjectMetaOptions `json:"options,omitempty"`
}

// ObjectInfo is meta plus instance information.
type ObjectInfo struct {
	ObjectMeta
	Bucket  string    `json:"bucket"`
	NUID    string    `json:"nuid"`
	Size    uint64    `json:"size"`
	ModTime time.Time `json:"mtime"`
	Chunks  uint32    `json:"chunks"`
	Digest  string    `json:"digest,omitempty"`
	Deleted bool      `json:"deleted,omitempty"`
}

// ObjectLink is used to embed links to other buckets and objects.
type ObjectLink struct {
	// Bucket is the name of the other object store.
	Bucket string `json:"bucket"`
	// Name can be used to link to a single object.
	// If empty means this is a link to the whole store, like a directory.
	Name string `json:"name,omitempty"`
}

// ObjectResult will return the underlying stream info and also be an io.ReadCloser.
type ObjectResult interface {
	io.ReadCloser
	Info() (*ObjectInfo, error)
	Error() error
}

const (
	objNameTmpl         = "OBJ_%s"     // OBJ_<bucket> // stream name
	objAllChunksPreTmpl = "$O.%s.C.>"  // $O.<bucket>.C.> // chunk stream subject
	objAllMetaPreTmpl   = "$O.%s.M.>"  // $O.<bucket>.M.> // meta stream subject
	objChunksPreTmpl    = "$O.%s.C.%s" // $O.<bucket>.C.<object-nuid> // chunk message subject
	objMetaPreTmpl      = "$O.%s.M.%s" // $O.<bucket>.M.<name-encoded> // meta message subject
	objNoPending        = "0"
	objDefaultChunkSize = uint32(128 * 1024) // 128k
	objDigestType       = "SHA-256="
	objDigestTmpl       = objDigestType + "%s"
)

type obs struct {
	name   string
	stream string
	js     *js
}

// CreateObjectStore will create an object store.
func (js *js) CreateObjectStore(cfg *ObjectStoreConfig) (ObjectStore, error) {
	if !js.nc.serverMinVersion(2, 6, 2) {
		return nil, ErrNeeds262
	}
	if cfg == nil {
		return nil, ErrObjectConfigRequired
	}
	if !validBucketRe.MatchString(cfg.Bucket) {
		return nil, ErrInvalidStoreName
	}

	name := cfg.Bucket
	chunks := fmt.Sprintf(objAllChunksPreTmpl, name)
	meta := fmt.Sprintf(objAllMetaPreTmpl, name)

	// We will set explicitly some values so that we can do comparison
	// if we get an "already in use" error and need to check if it is same.
	// See kv
	replicas := cfg.Replicas
	if replicas == 0 {
		replicas = 1
	}
	maxBytes := cfg.MaxBytes
	if maxBytes == 0 {
		maxBytes = -1
	}

	scfg := &StreamConfig{
		Name:        fmt.Sprintf(objNameTmpl, name),
		Description: cfg.Description,
		Subjects:    []string{chunks, meta},
		MaxAge:      cfg.TTL,
		MaxBytes:    maxBytes,
		Storage:     cfg.Storage,
		Replicas:    replicas,
		Placement:   cfg.Placement,
		Discard:     DiscardNew,
		AllowRollup: true,
		AllowDirect: true,
	}

	// Create our stream.
	_, err := js.AddStream(scfg)
	if err != nil {
		return nil, err
	}

	return &obs{name: name, stream: scfg.Name, js: js}, nil
}

// ObjectStore will look up and bind to an existing object store instance.
func (js *js) ObjectStore(bucket string) (ObjectStore, error) {
	if !validBucketRe.MatchString(bucket) {
		return nil, ErrInvalidStoreName
	}
	if !js.nc.serverMinVersion(2, 6, 2) {
		return nil, ErrNeeds262
	}

	stream := fmt.Sprintf(objNameTmpl, bucket)
	si, err := js.StreamInfo(stream)
	if err != nil {
		return nil, err
	}
	return &obs{name: bucket, stream: si.Config.Name, js: js}, nil
}

// DeleteObjectStore will delete the underlying stream for the named object.
func (js *js) DeleteObjectStore(bucket string) error {
	stream := fmt.Sprintf(objNameTmpl, bucket)
	return js.DeleteStream(stream)
}

func encodeName(name string) string {
	return base64.URLEncoding.EncodeToString([]byte(name))
}

// Put will place the contents from the reader into this object-store.
func (obs *obs) Put(meta *ObjectMeta, r io.Reader, opts ...ObjectOpt) (*ObjectInfo, error) {
	if meta == nil || meta.Name == "" {
		return nil, ErrBadObjectMeta
	}

	var o objOpts
	for _, opt := range opts {
		if opt != nil {
			if err := opt.configureObject(&o); err != nil {
				return nil, err
			}
		}
	}
	ctx := o.ctx

	// Create the new nuid so chunks go on a new subject if the name is re-used
	newnuid := nuid.Next()

	// These will be used in more than one place
	chunkSubj := fmt.Sprintf(objChunksPreTmpl, obs.name, newnuid)

	// Grab existing meta info (einfo). Ok to be found or not found, any other error is a problem
	// Chunks on the old nuid can be cleaned up at the end
	einfo, err := obs.GetInfo(meta.Name) // GetInfo will encode the name
	if err != nil && err != ErrObjectNotFound {
		return nil, err
	}

	// For async error handling
	var perr error
	var mu sync.Mutex
	setErr := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		perr = err
	}
	getErr := func() error {
		mu.Lock()
		defer mu.Unlock()
		return perr
	}

	purgePartial := func() { obs.js.purgeStream(obs.stream, &StreamPurgeRequest{Subject: chunkSubj}) }

	// Create our own JS context to handle errors etc.
	js, err := obs.js.nc.JetStream(PublishAsyncErrHandler(func(js JetStream, _ *Msg, err error) { setErr(err) }))
	if err != nil {
		return nil, err
	}

	chunkSize := objDefaultChunkSize
	if meta.Opts != nil && meta.Opts.ChunkSize > 0 {
		chunkSize = meta.Opts.ChunkSize
	}

	m, h := NewMsg(chunkSubj), sha256.New()
	chunk, sent, total := make([]byte, chunkSize), 0, uint64(0)

	// set up the info object. The chunk upload sets the size and digest
	info := &ObjectInfo{Bucket: obs.name, NUID: newnuid, ObjectMeta: *meta}

	for r != nil {
		if ctx != nil {
			select {
			case <-ctx.Done():
				if ctx.Err() == context.Canceled {
					err = ctx.Err()
				} else {
					err = ErrTimeout
				}
			default:
			}
			if err != nil {
				purgePartial()
				return nil, err
			}
		}

		// Actual read.
		// TODO(dlc) - Deadline?
		n, readErr := r.Read(chunk)

		// Handle all non EOF errors
		if readErr != nil && readErr != io.EOF {
			purgePartial()
			return nil, readErr
		}

		// Add chunk only if we received data
		if n > 0 {
			// Chunk processing.
			m.Data = chunk[:n]
			h.Write(m.Data)

			// Send msg itself.
			if _, err := js.PublishMsgAsync(m); err != nil {
				purgePartial()
				return nil, err
			}
			if err := getErr(); err != nil {
				purgePartial()
				return nil, err
			}
			// Update totals.
			sent++
			total += uint64(n)
		}

		// EOF Processing.
		if readErr == io.EOF {
			// Finalize sha.
			sha := h.Sum(nil)
			// Place meta info.
			info.Size, info.Chunks = uint64(total), uint32(sent)
			info.Digest = fmt.Sprintf(objDigestTmpl, base64.URLEncoding.EncodeToString(sha[:]))
			break
		}
	}

	// Prepare the meta message
	metaSubj := fmt.Sprintf(objMetaPreTmpl, obs.name, encodeName(meta.Name))
	mm := NewMsg(metaSubj)
	mm.Header.Set(MsgRollup, MsgRollupSubject)
	mm.Data, err = json.Marshal(info)
	if err != nil {
		if r != nil {
			purgePartial()
		}
		return nil, err
	}

	// Publish the meta message.
	_, err = js.PublishMsgAsync(mm)
	if err != nil {
		if r != nil {
			purgePartial()
		}
		return nil, err
	}

	// Wait for all to be processed.
	select {
	case <-js.PublishAsyncComplete():
		if err := getErr(); err != nil {
			if r != nil {
				purgePartial()
			}
			return nil, err
		}
	case <-time.After(obs.js.opts.wait):
		return nil, ErrTimeout
	}

	info.ModTime = time.Now().UTC() // This time is not actually the correct time

	// Delete any original chunks.
	if einfo != nil && !einfo.Deleted {
		echunkSubj := fmt.Sprintf(objChunksPreTmpl, obs.name, einfo.NUID)
		obs.js.purgeStream(obs.stream, &StreamPurgeRequest{Subject: echunkSubj})
	}

	// TODO would it be okay to do this to return the info with the correct time?
	// With the understanding that it is an extra call to the server.
	// Otherwise the time the user gets back is the client time, not the server time.
	// return obs.GetInfo(info.Name)

	return info, nil
}

// ObjectResult impl.
type objResult struct {
	sync.Mutex
	info   *ObjectInfo
	r      io.ReadCloser
	err    error
	ctx    context.Context
	digest hash.Hash
}

func (info *ObjectInfo) isLink() bool {
	return info.ObjectMeta.Opts != nil && info.ObjectMeta.Opts.Link != nil
}

// Get will pull the object from the underlying stream.
func (obs *obs) Get(name string, opts ...ObjectOpt) (ObjectResult, error) {
	// Grab meta info.
	info, err := obs.GetInfo(name)
	if err != nil {
		return nil, err
	}
	if info.NUID == _EMPTY_ {
		return nil, ErrBadObjectMeta
	}

	// Check for object links. If single objects we do a pass through.
	if info.isLink() {
		if info.ObjectMeta.Opts.Link.Name == _EMPTY_ {
			return nil, errors.New("nats: object is a link to a bucket")
		}

		// is the link in the same bucket?
		lbuck := info.ObjectMeta.Opts.Link.Bucket
		if lbuck == obs.name {
			return obs.Get(info.ObjectMeta.Opts.Link.Name)
		}

		// different bucket
		lobs, err := obs.js.ObjectStore(lbuck)
		if err != nil {
			return nil, err
		}
		return lobs.Get(info.ObjectMeta.Opts.Link.Name)
	}

	var o objOpts
	for _, opt := range opts {
		if opt != nil {
			if err := opt.configureObject(&o); err != nil {
				return nil, err
			}
		}
	}
	ctx := o.ctx

	result := &objResult{info: info, ctx: ctx}
	if info.Size == 0 {
		return result, nil
	}

	pr, pw := net.Pipe()
	result.r = pr

	gotErr := func(m *Msg, err error) {
		pw.Close()
		m.Sub.Unsubscribe()
		result.setErr(err)
	}

	// For calculating sum256
	result.digest = sha256.New()

	processChunk := func(m *Msg) {
		if ctx != nil {
			select {
			case <-ctx.Done():
				if ctx.Err() == context.Canceled {
					err = ctx.Err()
				} else {
					err = ErrTimeout
				}
			default:
			}
			if err != nil {
				gotErr(m, err)
				return
			}
		}

		tokens, err := getMetadataFields(m.Reply)
		if err != nil {
			gotErr(m, err)
			return
		}

		// Write to our pipe.
		for b := m.Data; len(b) > 0; {
			n, err := pw.Write(b)
			if err != nil {
				gotErr(m, err)
				return
			}
			b = b[n:]
		}
		// Update sha256
		result.digest.Write(m.Data)

		// Check if we are done.
		if tokens[ackNumPendingTokenPos] == objNoPending {
			pw.Close()
			m.Sub.Unsubscribe()
		}
	}

	chunkSubj := fmt.Sprintf(objChunksPreTmpl, obs.name, info.NUID)
	_, err = obs.js.Subscribe(chunkSubj, processChunk, OrderedConsumer())
	if err != nil {
		return nil, err
	}

	return result, nil
}

// Delete will delete the object.
func (obs *obs) Delete(name string) error {
	// Grab meta info.
	info, err := obs.GetInfo(name)
	if err != nil {
		return err
	}
	if info.NUID == _EMPTY_ {
		return ErrBadObjectMeta
	}

	// Place a rollup delete marker and publish the info
	info.Deleted = true
	info.Size, info.Chunks, info.Digest = 0, 0, _EMPTY_

	metaSubj := fmt.Sprintf(objMetaPreTmpl, obs.name, encodeName(name))
	mm := NewMsg(metaSubj)
	mm.Data, err = json.Marshal(info)
	if err != nil {
		return err
	}
	mm.Header.Set(MsgRollup, MsgRollupSubject)
	_, err = obs.js.PublishMsg(mm)
	if err != nil {
		return err
	}

	// Purge chunks for the object.
	chunkSubj := fmt.Sprintf(objChunksPreTmpl, obs.name, info.NUID)
	return obs.js.purgeStream(obs.stream, &StreamPurgeRequest{Subject: chunkSubj})
}

// AddLink will add a link to another object if it's not deleted and not another link
// name is the name of this link object
// obj is what is being linked too
func (obs *obs) AddLink(name string, obj *ObjectInfo) (*ObjectInfo, error) {
	if name == "" {
		return nil, ErrNameRequired
	}
	if obj == nil || obj.Name == "" {
		return nil, errors.New("nats: object required")
	}
	if obj.Deleted {
		return nil, errors.New("nats: not allowed to link to a deleted object")
	}
	if obj.isLink() {
		return nil, errors.New("nats: not allowed to link to another link")
	}

	// If object with link's name is found, error.
	// If link with link's name is found, that's okay to overwrite.
	// If there was an error that was not ErrObjectNotFound, error.
	einfo, err := obs.GetInfo(name)
	if einfo != nil {
		if !einfo.isLink() {
			return nil, ErrObjectAlreadyExists
		}
	} else if err != ErrObjectNotFound {
		return nil, err
	}

	// create the meta for the link
	meta := &ObjectMeta{
		Name: name,
		Opts: &ObjectMetaOptions{Link: &ObjectLink{Bucket: obj.Bucket, Name: obj.Name}},
	}

	// put the link object
	return obs.Put(meta, nil)
}

// AddBucketLink will add a link to another object store.
func (ob *obs) AddBucketLink(name string, bucket ObjectStore) (*ObjectInfo, error) {
	if name == "" {
		return nil, ErrNameRequired
	}
	if bucket == nil {
		return nil, errors.New("nats: bucket required")
	}
	bos, ok := bucket.(*obs)
	if !ok {
		return nil, errors.New("nats: bucket malformed")
	}

	// If object with link's name is found, error.
	// If link with link's name is found, that's okay to overwrite.
	// If there was an error that was not ErrObjectNotFound, error.
	einfo, err := ob.GetInfo(name)
	if einfo != nil {
		if !einfo.isLink() {
			return nil, ErrObjectAlreadyExists
		}
	} else if err != ErrObjectNotFound {
		return nil, err
	}

	// create the meta for the link
	meta := &ObjectMeta{
		Name: name,
		Opts: &ObjectMetaOptions{Link: &ObjectLink{Bucket: bos.name}},
	}

	// put the link object
	return ob.Put(meta, nil)
}

// PutBytes is convenience function to put a byte slice into this object store.
func (obs *obs) PutBytes(name string, data []byte, opts ...ObjectOpt) (*ObjectInfo, error) {
	return obs.Put(&ObjectMeta{Name: name}, bytes.NewReader(data), opts...)
}

// GetBytes is a convenience function to pull an object from this object store and return it as a byte slice.
func (obs *obs) GetBytes(name string, opts ...ObjectOpt) ([]byte, error) {
	result, err := obs.Get(name, opts...)
	if err != nil {
		return nil, err
	}
	defer result.Close()

	var b bytes.Buffer
	if _, err := b.ReadFrom(result); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// PutString is convenience function to put a string into this object store.
func (obs *obs) PutString(name string, data string, opts ...ObjectOpt) (*ObjectInfo, error) {
	return obs.Put(&ObjectMeta{Name: name}, strings.NewReader(data), opts...)
}

// GetString is a convenience function to pull an object from this object store and return it as a string.
func (obs *obs) GetString(name string, opts ...ObjectOpt) (string, error) {
	result, err := obs.Get(name, opts...)
	if err != nil {
		return _EMPTY_, err
	}
	defer result.Close()

	var b bytes.Buffer
	if _, err := b.ReadFrom(result); err != nil {
		return _EMPTY_, err
	}
	return b.String(), nil
}

// PutFile is convenience function to put a file into an object store.
func (obs *obs) PutFile(file string, opts ...ObjectOpt) (*ObjectInfo, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return obs.Put(&ObjectMeta{Name: file}, f, opts...)
}

// GetFile is a convenience function to pull and object and place in a file.
func (obs *obs) GetFile(name, file string, opts ...ObjectOpt) error {
	// Expect file to be new.
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	result, err := obs.Get(name, opts...)
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	defer result.Close()

	// Stream copy to the file.
	_, err = io.Copy(f, result)
	return err
}

// GetInfo will retrieve the current information for the object.
func (obs *obs) GetInfo(name string) (*ObjectInfo, error) {
	// Grab last meta value we have.
	if name == "" {
		return nil, ErrNameRequired
	}

	metaSubj := fmt.Sprintf(objMetaPreTmpl, obs.name, encodeName(name)) // used as data in a JS API call
	stream := fmt.Sprintf(objNameTmpl, obs.name)

	m, err := obs.js.GetLastMsg(stream, metaSubj)
	if err != nil {
		if err == ErrMsgNotFound {
			err = ErrObjectNotFound
		}
		return nil, err
	}
	var info ObjectInfo
	if err := json.Unmarshal(m.Data, &info); err != nil {
		return nil, ErrBadObjectMeta
	}
	info.ModTime = m.Time
	return &info, nil
}

// UpdateMeta will update the meta for the object.
func (obs *obs) UpdateMeta(name string, meta *ObjectMeta) error {
	if meta == nil {
		return ErrBadObjectMeta
	}

	// Grab the current meta.
	info, err := obs.GetInfo(name)
	if err != nil {
		return err
	}

	if info.Deleted {
		return errors.New("nats: cannot update meta for a deleted object")
	}

	// If the new name is different from the old, and it exists, error
	// If there was an error that was not ErrObjectNotFound, error.
	// sff - Is there a better go way to do this?
	if name != meta.Name {
		_, err = obs.GetInfo(meta.Name)
		if err != ErrObjectNotFound {
			if err == nil {
				return ErrObjectAlreadyExists
			}
			return err
		}
	}

	// Update Meta prevents update of ObjectMetaOptions (Link, ChunkSize)
	// These should only be updated internally when appropriate.
	info.Name = meta.Name
	info.Description = meta.Description
	info.Headers = meta.Headers

	// Prepare the meta message
	metaSubj := fmt.Sprintf(objMetaPreTmpl, obs.name, encodeName(meta.Name))
	mm := NewMsg(metaSubj)
	mm.Header.Set(MsgRollup, MsgRollupSubject)
	mm.Data, err = json.Marshal(info)
	if err != nil {
		return err
	}

	// Publish the meta message.
	_, err = obs.js.PublishMsg(mm)
	if err != nil {
		return err
	}

	// did the name of this object change? We just stored the meta under the new name
	// so delete the meta from the old name via purge stream for subject
	if name != meta.Name {
		metaSubj := fmt.Sprintf(objMetaPreTmpl, obs.name, encodeName(name))
		return obs.js.purgeStream(obs.stream, &StreamPurgeRequest{Subject: metaSubj})
	}

	return nil
}

// Seal will seal the object store, no further modifications will be allowed.
func (obs *obs) Seal() error {
	stream := fmt.Sprintf(objNameTmpl, obs.name)
	si, err := obs.js.StreamInfo(stream)
	if err != nil {
		return err
	}
	// Seal the stream from being able to take on more messages.
	cfg := si.Config
	cfg.Sealed = true
	_, err = obs.js.UpdateStream(&cfg)
	return err
}

// Implementation for Watch
type objWatcher struct {
	updates chan *ObjectInfo
	sub     *Subscription
}

// Updates returns the interior channel.
func (w *objWatcher) Updates() <-chan *ObjectInfo {
	if w == nil {
		return nil
	}
	return w.updates
}

// Stop will unsubscribe from the watcher.
func (w *objWatcher) Stop() error {
	if w == nil {
		return nil
	}
	return w.sub.Unsubscribe()
}

// Watch for changes in the underlying store and receive meta information updates.
func (obs *obs) Watch(opts ...WatchOpt) (ObjectWatcher, error) {
	var o watchOpts
	for _, opt := range opts {
		if opt != nil {
			if err := opt.configureWatcher(&o); err != nil {
				return nil, err
			}
		}
	}

	var initDoneMarker bool

	w := &objWatcher{updates: make(chan *ObjectInfo, 32)}

	update := func(m *Msg) {
		var info ObjectInfo
		if err := json.Unmarshal(m.Data, &info); err != nil {
			return // TODO(dlc) - Communicate this upwards?
		}
		meta, err := m.Metadata()
		if err != nil {
			return
		}

		if !o.ignoreDeletes || !info.Deleted {
			info.ModTime = meta.Timestamp
			w.updates <- &info
		}

		if !initDoneMarker && meta.NumPending == 0 {
			initDoneMarker = true
			w.updates <- nil
		}
	}

	allMeta := fmt.Sprintf(objAllMetaPreTmpl, obs.name)
	_, err := obs.js.GetLastMsg(obs.stream, allMeta)
	if err == ErrMsgNotFound {
		initDoneMarker = true
		w.updates <- nil
	}

	// Used ordered consumer to deliver results.
	subOpts := []SubOpt{OrderedConsumer()}
	if !o.includeHistory {
		subOpts = append(subOpts, DeliverLastPerSubject())
	}
	sub, err := obs.js.Subscribe(allMeta, update, subOpts...)
	if err != nil {
		return nil, err
	}
	w.sub = sub
	return w, nil
}

// List will list all the objects in this store.
func (obs *obs) List(opts ...WatchOpt) ([]*ObjectInfo, error) {
	opts = append(opts, IgnoreDeletes())
	watcher, err := obs.Watch(opts...)
	if err != nil {
		return nil, err
	}
	defer watcher.Stop()

	var objs []*ObjectInfo
	for entry := range watcher.Updates() {
		if entry == nil {
			break
		}
		objs = append(objs, entry)
	}
	if len(objs) == 0 {
		return nil, ErrNoObjectsFound
	}
	return objs, nil
}

// ObjectBucketStatus  represents status of a Bucket, implements ObjectStoreStatus
type ObjectBucketStatus struct {
	nfo    *StreamInfo
	bucket string
}

// Bucket is the name of the bucket
func (s *ObjectBucketStatus) Bucket() string { return s.bucket }

// Description is the description supplied when creating the bucket
func (s *ObjectBucketStatus) Description() string { return s.nfo.Config.Description }

// TTL indicates how long objects are kept in the bucket
func (s *ObjectBucketStatus) TTL() time.Duration { return s.nfo.Config.MaxAge }

// Storage indicates the underlying JetStream storage technology used to store data
func (s *ObjectBucketStatus) Storage() StorageType { return s.nfo.Config.Storage }

// Replicas indicates how many storage replicas are kept for the data in the bucket
func (s *ObjectBucketStatus) Replicas() int { return s.nfo.Config.Replicas }

// Sealed indicates the stream is sealed and cannot be modified in any way
func (s *ObjectBucketStatus) Sealed() bool { return s.nfo.Config.Sealed }

// Size is the combined size of all data in the bucket including metadata, in bytes
func (s *ObjectBucketStatus) Size() uint64 { return s.nfo.State.Bytes }

// BackingStore indicates what technology is used for storage of the bucket
func (s *ObjectBucketStatus) BackingStore() string { return "JetStream" }

// StreamInfo is the stream info retrieved to create the status
func (s *ObjectBucketStatus) StreamInfo() *StreamInfo { return s.nfo }

// Status retrieves run-time status about a bucket
func (obs *obs) Status() (ObjectStoreStatus, error) {
	nfo, err := obs.js.StreamInfo(obs.stream)
	if err != nil {
		return nil, err
	}

	status := &ObjectBucketStatus{
		nfo:    nfo,
		bucket: obs.name,
	}

	return status, nil
}

// Read impl.
func (o *objResult) Read(p []byte) (n int, err error) {
	o.Lock()
	defer o.Unlock()
	if ctx := o.ctx; ctx != nil {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				o.err = ctx.Err()
			} else {
				o.err = ErrTimeout
			}
		default:
		}
	}
	if o.err != nil {
		return 0, err
	}
	if o.r == nil {
		return 0, io.EOF
	}

	r := o.r.(net.Conn)
	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = r.Read(p)
	if err, ok := err.(net.Error); ok && err.Timeout() {
		if ctx := o.ctx; ctx != nil {
			select {
			case <-ctx.Done():
				if ctx.Err() == context.Canceled {
					return 0, ctx.Err()
				} else {
					return 0, ErrTimeout
				}
			default:
				err = nil
			}
		}
	}
	if err == io.EOF {
		// Make sure the digest matches.
		sha := o.digest.Sum(nil)
		digest := strings.SplitN(o.info.Digest, "=", 2)
		if len(digest) != 2 {
			o.err = ErrInvalidDigestFormat
			return 0, o.err
		}
		rsha, decodeErr := base64.URLEncoding.DecodeString(digest[1])
		if decodeErr != nil {
			o.err = decodeErr
			return 0, o.err
		}
		if !bytes.Equal(sha[:], rsha) {
			o.err = ErrDigestMismatch
			return 0, o.err
		}
	}
	return n, err
}

// Close impl.
func (o *objResult) Close() error {
	o.Lock()
	defer o.Unlock()
	if o.r == nil {
		return nil
	}
	return o.r.Close()
}

func (o *objResult) setErr(err error) {
	o.Lock()
	defer o.Unlock()
	o.err = err
}

func (o *objResult) Info() (*ObjectInfo, error) {
	o.Lock()
	defer o.Unlock()
	return o.info, o.err
}

func (o *objResult) Error() error {
	o.Lock()
	defer o.Unlock()
	return o.err
}
