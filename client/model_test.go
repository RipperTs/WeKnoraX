package client

import (
	"encoding/json"
	"testing"
)

func TestModelSortOrderJSONContract(t *testing.T) {
	var model Model
	if err := json.Unmarshal([]byte(`{"sort_order":25}`), &model); err != nil {
		t.Fatalf("unmarshal model: %v", err)
	}
	if model.SortOrder != 25 {
		t.Fatalf("SortOrder = %d, want 25", model.SortOrder)
	}

	zero := 0
	requests := map[string]interface{}{
		"create": CreateModelRequest{SortOrder: &zero},
		"update": UpdateModelRequest{SortOrder: &zero},
	}
	for name, request := range requests {
		t.Run(name+" preserves explicit zero", func(t *testing.T) {
			payload, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}

			var fields map[string]interface{}
			if err := json.Unmarshal(payload, &fields); err != nil {
				t.Fatalf("unmarshal request payload: %v", err)
			}
			if got, ok := fields["sort_order"]; !ok || got != float64(0) {
				t.Fatalf("sort_order = %v, present = %v, want explicit 0", got, ok)
			}
		})
	}

	omitted, err := json.Marshal(CreateModelRequest{})
	if err != nil {
		t.Fatalf("marshal request without sort order: %v", err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(omitted, &fields); err != nil {
		t.Fatalf("unmarshal request without sort order: %v", err)
	}
	if _, ok := fields["sort_order"]; ok {
		t.Fatal("sort_order should be omitted when unset")
	}
}
