package settings

import (
	"strings"
	"testing"
)

// TestSortedJSONMarshaling tests that the marshalSortedJSON function
// properly sorts struct fields and string arrays alphabetically
func TestSortedJSONMarshaling(t *testing.T) {
	// Create a test struct with unsorted fields and arrays
	testStruct := struct {
		ZField       string   `json:"z_field"`
		AField       string   `json:"a_field"`
		MField       string   `json:"m_field"`
		StringArray  []string `json:"string_array"`
		NestedStruct struct {
			YNested string `json:"y_nested"`
			BNested string `json:"b_nested"`
		} `json:"nested_struct"`
	}{
		ZField:      "z_value",
		AField:      "a_value",
		MField:      "m_value",
		StringArray: []string{"zebra", "apple", "banana"},
		NestedStruct: struct {
			YNested string `json:"y_nested"`
			BNested string `json:"b_nested"`
		}{
			YNested: "y_value",
			BNested: "b_value",
		},
	}

	// Marshal with our sorted function
	sortedJSON, err := marshalSortedJSON(testStruct, "  ")
	if err != nil {
		t.Fatalf("Failed to marshal sorted JSON: %v", err)
	}

	// Convert to string for easier testing
	sortedStr := string(sortedJSON)

	// Test that top-level fields are sorted
	aFieldPos := strings.Index(sortedStr, `"a_field"`)
	mFieldPos := strings.Index(sortedStr, `"m_field"`)
	zFieldPos := strings.Index(sortedStr, `"z_field"`)

	if aFieldPos == -1 || mFieldPos == -1 || zFieldPos == -1 {
		t.Fatal("Could not find expected fields in JSON output")
	}

	if !(aFieldPos < mFieldPos && mFieldPos < zFieldPos) {
		t.Error("Top-level fields are not sorted alphabetically")
	}

	// Test that string arrays are sorted (check for the sorted order in the formatted output)
	applePos := strings.Index(sortedStr, `"apple"`)
	bananaPos := strings.Index(sortedStr, `"banana"`)
	zebraPos := strings.Index(sortedStr, `"zebra"`)

	if applePos == -1 || bananaPos == -1 || zebraPos == -1 {
		t.Fatal("Could not find expected array elements in JSON output")
	}

	if !(applePos < bananaPos && bananaPos < zebraPos) {
		t.Error("String array is not sorted alphabetically")
	}

	// Test that nested struct fields are sorted
	nestedStart := strings.Index(sortedStr, `"nested_struct"`)
	if nestedStart == -1 {
		t.Fatal("Could not find nested_struct in JSON output")
	}

	nestedSection := sortedStr[nestedStart:]
	bNestedPos := strings.Index(nestedSection, `"b_nested"`)
	yNestedPos := strings.Index(nestedSection, `"y_nested"`)

	if bNestedPos == -1 || yNestedPos == -1 {
		t.Fatal("Could not find nested fields in JSON output")
	}

	if bNestedPos > yNestedPos {
		t.Error("Nested struct fields are not sorted alphabetically")
	}

	t.Logf("Sorted JSON output:\n%s", sortedStr)
}

// TestSortValueWithNilPointer tests that nil pointers are handled correctly
func TestSortValueWithNilPointer(t *testing.T) {
	var nilPtr *string
	result := sortValue(nilPtr)
	if result != nil {
		t.Error("Expected nil pointer to return nil")
	}
}

// TestSortValueWithEmptySlice tests that empty slices are handled correctly
func TestSortValueWithEmptySlice(t *testing.T) {
	emptySlice := []string{}
	result := sortValue(emptySlice)
	if result == nil {
		t.Error("Expected empty slice to return the slice, not nil")
	}
}
