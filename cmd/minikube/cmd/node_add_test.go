/*
Copyright 2024 The Kubernetes Authors All rights reserved.


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

package cmd

import (
	"testing"
)

func TestValidateOS(t *testing.T) {
	tests := []struct {
		osType   string
		errorMsg string
	}{
		{"linux", ""},
		{"windows", ""},
		{"foo", "Invalid OS: foo. Valid OS are: linux, windows"},
	}
	for _, test := range tests {
		t.Run(test.osType, func(t *testing.T) {
			got := validateOS(test.osType)
			gotError := ""
			if got != nil {
				gotError = got.Error()
			}
			if gotError != test.errorMsg {
				t.Errorf("validateOS(osType=%v): got %v, expected %v", test.osType, got, test.errorMsg)
			}
		})
	}
}
