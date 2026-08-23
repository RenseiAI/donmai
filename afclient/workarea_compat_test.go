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
)

func TestLegacyWorkareaStructLayoutsRemainFrozenForUnkeyedLiterals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		typeOf reflect.Type
		fields []string
	}{
		{
			name: "WorkareaSummary", typeOf: reflect.TypeOf(WorkareaSummary{}),
			fields: []string{
				"ID", "Kind", "ProviderID", "SessionID", "ProjectID", "Status", "Ref", "Repository",
				"CreatedAt", "SizeBytes", "SourceProvider", "Disposition", "AcquiredAt", "ReleasedAt", "AgeSeconds",
			},
		},
		{
			name: "Workarea", typeOf: reflect.TypeOf(Workarea{}),
			fields: []string{
				"ID", "Kind", "ProviderID", "SessionID", "ProjectID", "Status", "Path", "Ref", "Repository",
				"CleanStateChecksum", "Toolchain", "Mode", "AcquirePath", "AcquiredAt", "ReleasedAt",
				"ArchiveLocation", "OwnerSession", "Manifest",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.typeOf.NumField() != len(test.fields) {
				t.Fatalf("field count = %d, want frozen %d", test.typeOf.NumField(), len(test.fields))
			}
			for index, name := range test.fields {
				if got := test.typeOf.Field(index).Name; got != name {
					t.Fatalf("field[%d] = %q, want frozen %q", index, got, name)
				}
			}
		})
	}
}
