package actionscache_test

import (
	"bytes"
	"testing"

	cachepb "github.com/ysya/runscaler/internal/actionscache/pb/results/api/v1"
	entitypb "github.com/ysya/runscaler/internal/actionscache/pb/results/entities/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestCacheProtocolJSONAndBinary(t *testing.T) {
	request := &cachepb.FinalizeCacheEntryUploadRequest{}
	input := []byte(`{"metadata":{"repository_id":"42","scope":[{"scope":"refs/heads/main","permission":"3"}]},"key":"archive","version":"v1","size_bytes":"9007199254740993"}`)
	if err := protojson.Unmarshal(input, request); err != nil {
		t.Fatal(err)
	}
	if request.GetSizeBytes() != 9007199254740993 || request.GetMetadata().GetRepositoryId() != 42 || request.GetMetadata().GetScope()[0].GetPermission() != 3 {
		t.Fatalf("decoded wrong request: %v", request)
	}
	encoded, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded cachepb.FinalizeCacheEntryUploadRequest
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(request, &decoded) {
		t.Fatalf("binary round trip changed request: %v", &decoded)
	}
	jsonBytes, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(jsonBytes, []byte(`"size_bytes":"9007199254740993"`)) {
		t.Fatalf("invalid JSON round trip: %s", jsonBytes)
	}
	if got := (&entitypb.CacheMetadata{RepositoryId: 42}).GetRepositoryId(); got != 42 {
		t.Fatalf("metadata repository ID = %d", got)
	}
}
