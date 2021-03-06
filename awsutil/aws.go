// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package awsutil

import "net/http"

// Makes an http HEAD request using the URL provided.
// URL should either point to a public obejct or be
// a signed URL giving the user GET permissions.
func HeadObject(url string) (*http.Response, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Makes an http GET request using the URL provided.
// URL should either point to a public obejct or be
// a signed URL giving the user GET permissions.
// url: full url path to the object on AWS
func GetObject(url string) (*http.Response, error) {
	resp, err := GetObjectRange(url, "")
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Makes a ranged http GET request using the URL and byteRange provided.
// URL should either point to a public obejct or be
// a signed URL giving the user GET permissions.
// url: full url path to the object on AWS
// byteRange: the desired range of bytes of a file
// Should resemble the format for an http header Range.
// Example: "bytes="0-1000"
// Example: "bytes="1000-"
func GetObjectRange(url, byteRange string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if byteRange != "" {
		req.Header.Add("Range", byteRange)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func String(s string) *string {
	return &s
}

func Int64Value(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}
