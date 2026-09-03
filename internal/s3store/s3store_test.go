package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
)

func TestDeliverPublishesCompletionMarkerLastAndRetriesIdempotently(t *testing.T) {
	client := newFakeClient()
	store := &Store{client: client, bucket: "backups", prefix: "savetoa"}
	set, payload, manifestData, markerData := sourceSet(t)

	if err := store.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	wantKeys := []string{
		"savetoa/" + set.RelativePath + "/payload.tar",
		"savetoa/" + set.RelativePath + "/manifest.json",
		"savetoa/" + set.RelativePath + "/complete",
	}
	if strings.Join(client.puts, "\n") != strings.Join(wantKeys, "\n") {
		t.Fatalf("put order = %#v, want %#v", client.puts, wantKeys)
	}
	client.puts = nil
	if err := store.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
		t.Fatalf("Deliver(retry) error = %v", err)
	}
	if len(client.objects) != 3 {
		t.Fatalf("object count after retry = %d", len(client.objects))
	}
	if len(client.puts) != 0 {
		t.Fatalf("idempotent retry attempted PUTs: %#v", client.puts)
	}
}

func TestSDKDeliveryUsesTLSPathStyleSigningAndConditionalWrites(t *testing.T) {
	objects := make(map[string][]byte)
	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(request.URL.Path, "/backups/archive/") {
			t.Errorf("request path = %q", request.URL.Path)
		}
		if !strings.Contains(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("request is not SigV4 signed")
		}
		requests = append(requests, request.Method+" "+request.URL.Path)
		status := http.StatusOK
		var responseBody []byte
		switch request.Method {
		case http.MethodPut:
			if request.Header.Get("If-None-Match") != "*" {
				t.Errorf("If-None-Match = %q", request.Header.Get("If-None-Match"))
			}
			if _, exists := objects[request.URL.Path]; exists {
				status = http.StatusPreconditionFailed
				break
			}
			data, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			objects[request.URL.Path] = data
		case http.MethodGet:
			data, exists := objects[request.URL.Path]
			if !exists {
				status = http.StatusNotFound
				break
			}
			responseBody = data
		default:
			status = http.StatusMethodNotAllowed
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
			Request:    request,
		}, nil
	})}
	store, err := New(Options{
		Endpoint: "https://s3.test.invalid", Region: "test-region-1", Bucket: "backups", Prefix: "archive",
		Credentials: Credentials{AccessKeyID: "test-access", SecretAccessKey: "test-secret"},
		HTTPClient:  client,
	})
	if err != nil {
		t.Fatal(err)
	}
	set, payload, manifestData, markerData := sourceSet(t)
	if err := store.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if len(requests) != 9 || !strings.HasPrefix(requests[7], "PUT ") || !strings.HasSuffix(requests[7], "/complete") || !strings.HasSuffix(requests[8], "/complete") {
		t.Fatalf("requests = %#v", requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestDeliverRejectsConflictWithoutPublishingCompletionMarker(t *testing.T) {
	client := newFakeClient()
	store := &Store{client: client, bucket: "backups"}
	set, payload, manifestData, markerData := sourceSet(t)
	payloadKey := set.RelativePath + "/payload.tar"
	client.objects[payloadKey] = []byte("different")

	err := store.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Deliver() error = %v", err)
	}
	if _, ok := client.objects[set.RelativePath+"/complete"]; ok {
		t.Fatal("completion marker was published after a conflict")
	}
}

func TestDeliverFailureBeforeMarkerLeavesSetIncomplete(t *testing.T) {
	client := newFakeClient()
	store := &Store{client: client, bucket: "backups"}
	set, payload, manifestData, markerData := sourceSet(t)
	client.failKey = set.RelativePath + "/manifest.json"

	if err := store.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err == nil {
		t.Fatal("Deliver() succeeded")
	}
	if _, ok := client.objects[set.RelativePath+"/payload.tar"]; !ok {
		t.Fatal("payload was not retained for retry")
	}
	if _, ok := client.objects[set.RelativePath+"/complete"]; ok {
		t.Fatal("completion marker was published after manifest failure")
	}
}

func TestListAndDeleteCompletedIgnoreIncompleteAndRemoveMarkerFirst(t *testing.T) {
	client := newFakeClient()
	store := &Store{client: client, bucket: "backups", prefix: "savetoa"}
	set, payload, manifestData, markerData := sourceSet(t)
	if err := store.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
		t.Fatal(err)
	}
	client.objects["savetoa/production/database/2026/09/03/incomplete/payload.tar"] = []byte("incomplete")
	sets, err := store.ListCompleted(context.Background(), "production", "database")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || sets[0].RelativePath != set.RelativePath {
		t.Fatalf("completed sets = %#v", sets)
	}
	if err := store.DeleteCompleted(context.Background(), sets[0]); err != nil {
		t.Fatal(err)
	}
	wantDeletes := []string{
		store.key(set.RelativePath, localstore.CompleteFilename),
		store.key(set.RelativePath, set.Manifest.Artifact.Filename),
		store.key(set.RelativePath, localstore.ManifestFilename),
	}
	if strings.Join(client.deletes, "\n") != strings.Join(wantDeletes, "\n") {
		t.Fatalf("delete order = %#v, want %#v", client.deletes, wantDeletes)
	}
	if _, exists := client.objects["savetoa/production/database/2026/09/03/incomplete/payload.tar"]; !exists {
		t.Fatal("incomplete S3 set was modified")
	}
}

func TestListCompletedFailsClosedBeforeDeletingInvalidSet(t *testing.T) {
	client := newFakeClient()
	store := &Store{client: client, bucket: "backups"}
	set, payload, manifestData, markerData := sourceSet(t)
	if err := store.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
		t.Fatal(err)
	}
	client.objects[store.key(set.RelativePath, localstore.CompleteFilename)] = []byte("invalid\n")
	if _, err := store.ListCompleted(context.Background(), "production", "database"); err == nil {
		t.Fatal("invalid completed S3 set was accepted")
	}
	if len(client.deletes) != 0 {
		t.Fatalf("failed scan deleted objects: %#v", client.deletes)
	}
}

func TestListCompletedReportsOnlySafeHTTPStatus(t *testing.T) {
	client := newFakeClient()
	client.listError = responseError(http.StatusForbidden)
	store := &Store{client: client, bucket: "backups"}

	_, err := store.ListCompleted(context.Background(), "production", "database")
	if err == nil || err.Error() != "list S3 retention candidates: S3 returned HTTP 403" {
		t.Fatalf("ListCompleted() error = %v", err)
	}
}

func TestFetchVerifiesAndAtomicallyImportsCompletedSet(t *testing.T) {
	client := newFakeClient()
	remote := &Store{client: client, bucket: "backups", prefix: "savetoa"}
	set, payload, manifestData, markerData := sourceSet(t)
	if err := remote.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
		t.Fatal(err)
	}

	destinationRoot := t.TempDir()
	destination, err := localstore.New(destinationRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	client.gets = nil
	fetched, err := remote.Fetch(context.Background(), set.RelativePath, destination)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if fetched.RelativePath != set.RelativePath {
		t.Fatalf("Fetch() path = %q, want %q", fetched.RelativePath, set.RelativePath)
	}
	if _, err := destination.Verify(context.Background(), fetched.RelativePath); err != nil {
		t.Fatalf("Verify(fetched) error = %v", err)
	}
	wantGets := []string{
		remote.key(set.RelativePath, localstore.CompleteFilename),
		remote.key(set.RelativePath, localstore.ManifestFilename),
		remote.key(set.RelativePath, set.Manifest.Artifact.Filename),
	}
	if strings.Join(client.gets, "\n") != strings.Join(wantGets, "\n") {
		t.Fatalf("GET order = %#v, want %#v", client.gets, wantGets)
	}

	client.gets = nil
	if _, err := remote.Fetch(context.Background(), set.RelativePath, destination); err != nil {
		t.Fatalf("Fetch(retry) error = %v", err)
	}
	if len(client.gets) != 2 {
		t.Fatalf("idempotent fetch made %d GETs, want metadata only", len(client.gets))
	}
}

func TestFetchRejectsCorruptionWithoutPublishingLocalSet(t *testing.T) {
	tests := map[string]func(*fakeClient, *Store, *localstore.Set){
		"marker": func(client *fakeClient, remote *Store, set *localstore.Set) {
			client.objects[remote.key(set.RelativePath, localstore.CompleteFilename)] = []byte("sha256:" + strings.Repeat("0", 64) + "\n")
		},
		"payload": func(client *fakeClient, remote *Store, set *localstore.Set) {
			client.objects[remote.key(set.RelativePath, set.Manifest.Artifact.Filename)] = []byte("changed")
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			client := newFakeClient()
			remote := &Store{client: client, bucket: "backups"}
			set, payload, manifestData, markerData := sourceSet(t)
			if err := remote.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
				t.Fatal(err)
			}
			corrupt(client, remote, set)
			destinationRoot := t.TempDir()
			destination, err := localstore.New(destinationRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer destination.Close()
			if _, err := remote.Fetch(context.Background(), set.RelativePath, destination); err == nil {
				t.Fatal("Fetch(corrupt) succeeded")
			}
			if _, err := destination.Load(set.RelativePath); err == nil {
				t.Fatal("corrupt remote set was published locally")
			}
		})
	}
}

func TestFetchRejectsInvalidPathBeforeRemoteAccess(t *testing.T) {
	client := newFakeClient()
	remote := &Store{client: client, bucket: "backups"}
	destination, err := localstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	if _, err := remote.Fetch(context.Background(), "../escape", destination); err == nil {
		t.Fatal("Fetch(invalid path) succeeded")
	}
	if len(client.gets) != 0 {
		t.Fatalf("invalid path caused remote reads: %#v", client.gets)
	}
}

func TestFetchRejectsNonCanonicalManifest(t *testing.T) {
	client := newFakeClient()
	remote := &Store{client: client, bucket: "backups"}
	set, payload, manifestData, markerData := sourceSet(t)
	if err := remote.Deliver(context.Background(), set.RelativePath, set, payload, manifestData, markerData); err != nil {
		t.Fatal(err)
	}
	nonCanonical := append([]byte(" "), manifestData...)
	digest := sha256.Sum256(nonCanonical)
	client.objects[remote.key(set.RelativePath, localstore.ManifestFilename)] = nonCanonical
	client.objects[remote.key(set.RelativePath, localstore.CompleteFilename)] = []byte("sha256:" + hex.EncodeToString(digest[:]) + "\n")
	destination, err := localstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	if _, err := remote.Fetch(context.Background(), set.RelativePath, destination); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("Fetch(non-canonical) error = %v", err)
	}
}

func TestReadCredentialsFailsClosedWithoutLeakingValues(t *testing.T) {
	secret := "do-not-print-this"
	tests := []string{
		"access_key_id: key\nsecret_access_key: " + secret + "\nunknown: true\n",
		"access_key_id: key\naccess_key_id: duplicate\nsecret_access_key: " + secret + "\n",
		"access_key_id: key\nsecret_access_key: " + secret + "\n---\n{}\n",
		"access_key_id: key\nsecret_access_key: |\n  " + secret + "\n  second-line\n",
	}
	for _, input := range tests {
		_, err := ReadCredentials(strings.NewReader(input))
		if err == nil {
			t.Fatalf("ReadCredentials(%q) succeeded", input)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked secret: %v", err)
		}
	}
}

func sourceSet(t *testing.T) (*localstore.Set, io.ReadSeeker, []byte, []byte) {
	t.Helper()
	root := t.TempDir()
	store, err := localstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	value := manifest.Manifest{
		FormatVersion: manifest.FormatVersion,
		BackupID:      "20260902-s3-example", Target: "database", CaptureDriver: "mariadb",
		StartedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 9, 2, 10, 1, 0, 0, time.UTC),
		Tool:     manifest.Tool{Name: "mariadb-backup", Version: "12.3.3"},
		Source:   manifest.Source{ServerVersion: "12.3.3", Replication: map[string]string{"gtid": "0-1-2"}},
		Artifact: manifest.Artifact{Filename: "payload.tar"}, Transformations: []manifest.Transformation{},
	}
	set, err := store.Commit(context.Background(), "production", value, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	verified, payload, err := store.OpenVerifiedPayload(context.Background(), set.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = payload.Close() })
	metadataSet, manifestData, markerData, err := store.ReadMetadata(set.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if metadataSet.Manifest.Artifact != verified.Manifest.Artifact {
		t.Fatal("metadata changed between verification and read")
	}
	return verified, payload, manifestData, markerData
}

type fakeClient struct {
	objects   map[string][]byte
	puts      []string
	gets      []string
	deletes   []string
	failKey   string
	listError error
}

func (client *fakeClient) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if client.listError != nil {
		return nil, client.listError
	}
	prefix := aws.ToString(input.Prefix)
	keys := make([]string, 0, len(client.objects))
	for key := range client.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	objects := make([]types.Object, len(keys))
	for index, key := range keys {
		objects[index] = types.Object{Key: aws.String(key), Size: aws.Int64(int64(len(client.objects[key])))}
	}
	return &s3.ListObjectsV2Output{Contents: objects, IsTruncated: aws.Bool(false)}, nil
}

func (client *fakeClient) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	key := aws.ToString(input.Key)
	client.deletes = append(client.deletes, key)
	if key == client.failKey {
		return nil, errors.New("injected delete failure")
	}
	delete(client.objects, key)
	return &s3.DeleteObjectOutput{}, nil
}

func newFakeClient() *fakeClient { return &fakeClient{objects: make(map[string][]byte)} }

func (client *fakeClient) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	key := aws.ToString(input.Key)
	client.puts = append(client.puts, key)
	if key == client.failKey {
		return nil, errors.New("injected failure containing credentials that must not escape")
	}
	if _, ok := client.objects[key]; ok {
		return nil, responseError(http.StatusPreconditionFailed)
	}
	data, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	client.objects[key] = data
	return &s3.PutObjectOutput{}, nil
}

func (client *fakeClient) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(input.Key)
	client.gets = append(client.gets, key)
	data, ok := client.objects[key]
	if !ok {
		return nil, responseError(http.StatusNotFound)
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: aws.Int64(int64(len(data)))}, nil
}

func responseError(status int) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      errors.New("S3 response error"),
	}
}
