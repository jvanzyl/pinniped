// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"io/ioutil"
	"os"
	"testing"

	"k8s.io/utils/pointer"

	"github.com/stretchr/testify/require"

	"go.pinniped.dev/internal/here"
)

func TestFromPath(t *testing.T) {
	tests := []struct {
		name       string
		yaml       string
		wantConfig *Config
		wantError  string
	}{
		{
			name: "Happy",
			yaml: here.Doc(`
				---
				apiGroupSuffix: some.suffix.com
				labels:
				  myLabelKey1: myLabelValue1
				  myLabelKey2: myLabelValue2
				names:
				  defaultTLSCertificateSecret: my-secret-name
				endpoints:
				  https:
				    network: unix
				    address: :1234
				  http:
				    network: disabled
			`),
			wantConfig: &Config{
				APIGroupSuffix: pointer.StringPtr("some.suffix.com"),
				Labels: map[string]string{
					"myLabelKey1": "myLabelValue1",
					"myLabelKey2": "myLabelValue2",
				},
				NamesConfig: NamesConfigSpec{
					DefaultTLSCertificateSecret: "my-secret-name",
				},
				Endpoints: &Endpoints{
					HTTPS: &Endpoint{
						Network: "unix",
						Address: ":1234",
					},
					HTTP: &Endpoint{
						Network: "disabled",
					},
				},
			},
		},
		{
			name: "When only the required fields are present, causes other fields to be defaulted",
			yaml: here.Doc(`
				---
				names:
				  defaultTLSCertificateSecret: my-secret-name
			`),
			wantConfig: &Config{
				APIGroupSuffix: pointer.StringPtr("pinniped.dev"),
				Labels:         map[string]string{},
				NamesConfig: NamesConfigSpec{
					DefaultTLSCertificateSecret: "my-secret-name",
				},
				Endpoints: &Endpoints{
					HTTPS: &Endpoint{
						Network: "tcp",
						Address: ":8443",
					},
					HTTP: &Endpoint{
						Network: "tcp",
						Address: ":8080",
					},
				},
			},
		},
		{
			name: "all endpoints disabled",
			yaml: here.Doc(`
				---
				names:
				  defaultTLSCertificateSecret: my-secret-name
				endpoints:
				  https:
				    network: disabled
				  http:
				    network: disabled
			`),
			wantError: "validate endpoints: all endpoints are disabled",
		},
		{
			name: "invalid https endpoint",
			yaml: here.Doc(`
				---
				names:
				  defaultTLSCertificateSecret: my-secret-name
				endpoints:
				  https:
				    network: foo
				  http:
				    network: disabled
			`),
			wantError: `validate https endpoint: unknown network "foo"`,
		},
		{
			name: "invalid http endpoint",
			yaml: here.Doc(`
				---
				names:
				  defaultTLSCertificateSecret: my-secret-name
				endpoints:
				  https:
				    network: disabled
				  http:
				    network: bar
			`),
			wantError: `validate http endpoint: unknown network "bar"`,
		},
		{
			name: "endpoint disabled with non-empty address",
			yaml: here.Doc(`
				---
				names:
				  defaultTLSCertificateSecret: my-secret-name
				endpoints:
				  https:
				    network: disabled
				    address: wee
			`),
			wantError: `validate https endpoint: address set to "wee" when disabled, should be empty`,
		},
		{
			name: "endpoint tcp with empty address",
			yaml: here.Doc(`
				---
				names:
				  defaultTLSCertificateSecret: my-secret-name
				endpoints:
				  http:
				    network: tcp
			`),
			wantError: `validate http endpoint: address must be set with "tcp" network`,
		},
		{
			name: "endpoint unix with empty address",
			yaml: here.Doc(`
				---
				names:
				  defaultTLSCertificateSecret: my-secret-name
				endpoints:
				  https:
				    network: unix
			`),
			wantError: `validate https endpoint: address must be set with "unix" network`,
		},
		{
			name: "Missing defaultTLSCertificateSecret name",
			yaml: here.Doc(`
				---
			`),
			wantError: "validate names: missing required names: defaultTLSCertificateSecret",
		},
		{
			name: "apiGroupSuffix is prefixed with '.'",
			yaml: here.Doc(`
				---
				apiGroupSuffix: .starts.with.dot
				names:
				  defaultTLSCertificateSecret: my-secret-name
			`),
			wantError: "validate apiGroupSuffix: a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			// Write yaml to temp file
			f, err := ioutil.TempFile("", "pinniped-test-config-yaml-*")
			require.NoError(t, err)
			defer func() {
				err := os.Remove(f.Name())
				require.NoError(t, err)
			}()
			_, err = f.WriteString(test.yaml)
			require.NoError(t, err)
			err = f.Close()
			require.NoError(t, err)

			// Test FromPath()
			config, err := FromPath(f.Name())

			if test.wantError != "" {
				require.EqualError(t, err, test.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.wantConfig, config)
			}
		})
	}
}
