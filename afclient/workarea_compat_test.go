package afclient

import (
	"reflect"
	"testing"
)

// These positional literals intentionally compile against the exact released
// layouts. Keep them unkeyed: their purpose is to fail at compile time if a
// field is inserted, removed, reordered, or retyped.
var (
	_ = WorkareaSummary{
		"", WorkareaKind(""), "", "", "", WorkareaPoolStatus(""), "", "",
		nil, 0, "", "", nil, nil, 0,
	}
	_ = Workarea{
		"", WorkareaKind(""), "", "", "", WorkareaPoolStatus(""), "", "", "", "",
		nil, "", "", nil, nil, "", "", nil,
	}
	_ = ListWorkareasResponse{nil, nil}
	_ = WorkareaEnvelope{Workarea{}}
	_ = WorkareaRestoreResult{Workarea{}}
)

func TestLegacyWorkareaStructLayoutsRemainFrozenForUnkeyedLiterals(t *testing.T) {
	t.Parallel()
	type field struct{ name, jsonTag string }
	f := func(name, jsonTag string) field { return field{name: name, jsonTag: jsonTag} }
	tests := []struct {
		name   string
		typeOf reflect.Type
		fields []field
	}{
		{
			name: "WorkareaSummary", typeOf: reflect.TypeOf(WorkareaSummary{}),
			fields: []field{
				f("ID", "id"), f("Kind", "kind"), f("ProviderID", "providerId"), f("SessionID", "sessionId,omitempty"),
				f("ProjectID", "projectId,omitempty"), f("Status", "status"), f("Ref", "ref,omitempty"), f("Repository", "repository,omitempty"),
				f("CreatedAt", "createdAt,omitempty"), f("SizeBytes", "sizeBytes,omitempty"), f("SourceProvider", "sourceProvider,omitempty"),
				f("Disposition", "disposition,omitempty"), f("AcquiredAt", "acquiredAt,omitempty"), f("ReleasedAt", "releasedAt,omitempty"),
				f("AgeSeconds", "ageSeconds,omitempty"),
			},
		},
		{
			name: "Workarea", typeOf: reflect.TypeOf(Workarea{}),
			fields: []field{
				f("ID", "id"), f("Kind", "kind"), f("ProviderID", "providerId"), f("SessionID", "sessionId,omitempty"),
				f("ProjectID", "projectId,omitempty"), f("Status", "status"), f("Path", "path,omitempty"), f("Ref", "ref,omitempty"),
				f("Repository", "repository,omitempty"), f("CleanStateChecksum", "cleanStateChecksum,omitempty"), f("Toolchain", "toolchain,omitempty"),
				f("Mode", "mode,omitempty"), f("AcquirePath", "acquirePath,omitempty"), f("AcquiredAt", "acquiredAt,omitempty"),
				f("ReleasedAt", "releasedAt,omitempty"), f("ArchiveLocation", "archiveLocation,omitempty"),
				f("OwnerSession", "ownerSession,omitempty"), f("Manifest", "manifest,omitempty"),
			},
		},
		{
			name: "ListWorkareasResponse", typeOf: reflect.TypeOf(ListWorkareasResponse{}),
			fields: []field{f("Active", "active"), f("Archived", "archived")},
		},
		{
			name: "WorkareaEnvelope", typeOf: reflect.TypeOf(WorkareaEnvelope{}),
			fields: []field{f("Workarea", "workarea")},
		},
		{
			name: "WorkareaRestoreResult", typeOf: reflect.TypeOf(WorkareaRestoreResult{}),
			fields: []field{f("Workarea", "workarea")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.typeOf.NumField() != len(test.fields) {
				t.Fatalf("field count = %d, want frozen %d", test.typeOf.NumField(), len(test.fields))
			}
			for index, expected := range test.fields {
				actual := test.typeOf.Field(index)
				if actual.Name != expected.name || actual.Tag.Get("json") != expected.jsonTag {
					t.Fatalf("field[%d] = %q json=%q, want frozen %q json=%q", index, actual.Name, actual.Tag.Get("json"), expected.name, expected.jsonTag)
				}
			}
		})
	}
}
