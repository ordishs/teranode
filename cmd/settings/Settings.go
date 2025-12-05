// Package settings provide utilities for managing and displaying application settings.
// It is intended for debugging and inspection of application configuration and runtime behavior.
//
// Usage:
//
// This package is typically used to print application settings in JSON format,
// along-with-version and commit information, for debugging and documentation purposes.
//
// Functions:
//   - PrintSettings: Prints application settings, version, and commit information in a structured format.
//
// Side effects:
//
// Functions in this package may print to stdout and log errors if they occur.
package settings

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/ordishs/gocore"
)

// marshalSortedJSON marshals a struct to JSON with sorted keys at all levels.
// It recursively sorts struct fields and string slices alphabetically.
func marshalSortedJSON(v interface{}, indent string) ([]byte, error) {
	return json.MarshalIndent(sortValue(v), "", indent)
}

// sortValue recursively sorts struct fields and string slices
func sortValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}

	val := reflect.ValueOf(v)
	typ := reflect.TypeOf(v)

	// Handle pointers
	if val.Kind() == reflect.Ptr {
		if val.IsNil() {
			return nil
		}
		return sortValue(val.Elem().Interface())
	}

	// Handle slices
	if val.Kind() == reflect.Slice {
		if val.Len() == 0 {
			return v
		}

		// For string slices, sort them
		if val.Type().Elem().Kind() == reflect.String {
			sorted := make([]string, val.Len())
			for i := 0; i < val.Len(); i++ {
				sorted[i] = val.Index(i).String()
			}
			sort.Strings(sorted)
			return sorted
		}

		// For other slices, recursively sort elements
		result := make([]interface{}, val.Len())
		for i := 0; i < val.Len(); i++ {
			result[i] = sortValue(val.Index(i).Interface())
		}
		return result
	}

	// Handle structs
	if val.Kind() == reflect.Struct {
		result := make(map[string]interface{})

		for i := 0; i < val.NumField(); i++ {
			field := typ.Field(i)
			fieldVal := val.Field(i)

			// Skip unexported fields
			if !fieldVal.CanInterface() {
				continue
			}

			// Get JSON tag name, fallback to field name
			jsonTag := field.Tag.Get("json")
			fieldName := field.Name
			if jsonTag != "" && jsonTag != "-" {
				// Handle json:",omitempty" and similar
				if idx := strings.Index(jsonTag, ","); idx != -1 {
					fieldName = jsonTag[:idx]
				} else {
					fieldName = jsonTag
				}
			}

			result[fieldName] = sortValue(fieldVal.Interface())
		}
		return result
	}

	// For all other types, return as-is
	return v
}

// PrintSettings prints the application settings, version, and commit information in a structured format.
//
// This function is used to display the current application configuration and runtime details
// for debugging and documentation purposes. It outputs the settings in JSON format along with
// version and commit metadata.
//
// Parameters:
//   - logger: A logger instance for logging errors.
//   - settings: The application settings to be displayed.
//   - version: The version of the application.
//   - commit: The commit hash of the application.
//
// Side effects:
//   - Prints the settings, version, and commit information to stdout.
//   - Logs errors if the settings cannot be marshaled to JSON.
func PrintSettings(logger ulogger.Logger, settings *settings.Settings, version, commit string) {
	stats := gocore.Config().Stats()
	logger.Infof("STATS\n%s\nVERSION\n-------\n%s (%s)\n\n", stats, version, commit)

	settingsJSON, err := marshalSortedJSON(settings, "  ")
	if err != nil {
		logger.Errorf("Failed to marshal settings: %v", err)
	} else {
		logger.Infof("SETTINGS JSON\n-------------\n%s\n\n", string(settingsJSON))
	}
}
