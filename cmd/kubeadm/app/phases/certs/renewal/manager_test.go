/*
Copyright 2019 The Kubernetes Authors.

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

package renewal

import (
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	certutil "k8s.io/client-go/util/cert"
	netutils "k8s.io/utils/net"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	certtestutil "k8s.io/kubernetes/cmd/kubeadm/app/util/certs"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/pkiutil"
	testutil "k8s.io/kubernetes/cmd/kubeadm/test"
)

var (
	testCACertCfg = &pkiutil.CertConfig{
		Config: certutil.Config{CommonName: "kubernetes"},
	}

	testCACert, testCAKey, _ = pkiutil.NewCertificateAuthority(testCACertCfg)

	testCertOrganization = []string{"sig-cluster-lifecycle"}

	testCertCfg = makeTestCertConfig(testCertOrganization)
)

type fakecertificateReadWriter struct {
	exist bool
	cert  *x509.Certificate
}

func (cr fakecertificateReadWriter) Exists() bool {
	return cr.exist
}

func (cr fakecertificateReadWriter) Read() (*x509.Certificate, error) {
	return cr.cert, nil
}

func (cr fakecertificateReadWriter) Write(*x509.Certificate, crypto.Signer) error {
	return nil
}

func TestNewManager(t *testing.T) {
	tests := []struct {
		name                 string
		cfg                  *kubeadmapi.ClusterConfiguration
		expectedCertificates int
	}{
		{
			name:                 "cluster with local etcd",
			cfg:                  &kubeadmapi.ClusterConfiguration{},
			expectedCertificates: 11, // [admin super-admin apiserver apiserver-etcd-client apiserver-kubelet-client controller-manager etcd/healthcheck-client etcd/peer etcd/server front-proxy-client scheduler]
		},
		{
			name: "cluster with external etcd",
			cfg: &kubeadmapi.ClusterConfiguration{
				Etcd: kubeadmapi.Etcd{
					External: &kubeadmapi.ExternalEtcd{},
				},
			},
			expectedCertificates: 7, // [admin super-admin apiserver apiserver-kubelet-client controller-manager front-proxy-client scheduler]
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rm, err := NewManager(test.cfg, "")
			if err != nil {
				t.Fatalf("Failed to create the certificate renewal manager: %v", err)
			}

			if len(rm.Certificates()) != test.expectedCertificates {
				t.Errorf("Expected %d certificates, saw %d", test.expectedCertificates, len(rm.Certificates()))
			}
		})
	}
}

func TestRenewUsingLocalCA(t *testing.T) {
	dir := testutil.SetupTempDir(t)
	defer os.RemoveAll(dir)

	if err := pkiutil.WriteCertAndKey(dir, "ca", testCACert, testCAKey); err != nil {
		t.Fatalf("couldn't write out CA certificate to %s", dir)
	}

	etcdDir := filepath.Join(dir, "etcd")
	if err := pkiutil.WriteCertAndKey(etcdDir, "ca", testCACert, testCAKey); err != nil {
		t.Fatalf("couldn't write out CA certificate to %s", etcdDir)
	}

	cfg := &kubeadmapi.ClusterConfiguration{
		CertificatesDir: dir,
	}
	rm, err := NewManager(cfg, dir)
	if err != nil {
		t.Fatalf("Failed to create the certificate renewal manager: %v", err)
	}

	tests := []struct {
		name                 string
		certName             string
		createCertFunc       func() *x509.Certificate
		expectedOrganization []string
	}{
		{
			name:     "Certificate renewal for a PKI certificate",
			certName: "apiserver",
			createCertFunc: func() *x509.Certificate {
				return writeTestCertificate(t, dir, "apiserver", testCACert, testCAKey, testCertOrganization)
			},
			expectedOrganization: testCertOrganization,
		},
		{
			name:     "Certificate renewal for a certificate embedded in a kubeconfig file",
			certName: "admin.conf",
			createCertFunc: func() *x509.Certificate {
				return writeTestKubeconfig(t, dir, "admin.conf", testCACert, testCAKey)
			},
			expectedOrganization: testCertOrganization,
		},
		{
			name:     "apiserver-etcd-client cert should not contain SystemPrivilegedGroup after renewal",
			certName: "apiserver-etcd-client",
			createCertFunc: func() *x509.Certificate {
				return writeTestCertificate(t, dir, "apiserver-etcd-client", testCACert, testCAKey, []string{kubeadmconstants.SystemPrivilegedGroup})
			},
			expectedOrganization: []string{},
		},
		{
			name:     "apiserver-kubelet-client cert should replace SystemPrivilegedGroup with ClusterAdminsGroup after renewal",
			certName: "apiserver-kubelet-client",
			createCertFunc: func() *x509.Certificate {
				return writeTestCertificate(t, dir, "apiserver-kubelet-client", testCACert, testCAKey, []string{kubeadmconstants.SystemPrivilegedGroup})
			},
			expectedOrganization: []string{kubeadmconstants.ClusterAdminsGroupAndClusterRoleBinding},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cert := test.createCertFunc()

			time.Sleep(1 * time.Second)

			_, err := rm.RenewUsingLocalCA(test.certName)
			if err != nil {
				t.Fatalf("error renewing certificate: %v", err)
			}

			newCert, err := rm.certificates[test.certName].readwriter.Read()
			if err != nil {
				t.Fatalf("error reading renewed certificate: %v", err)
			}

			if newCert.SerialNumber.Cmp(cert.SerialNumber) == 0 {
				t.Fatal("expected new certificate, but renewed certificate has same serial number")
			}

			if !newCert.NotAfter.After(cert.NotAfter) {
				t.Fatalf("expected new certificate with updated expiration, but renewed certificate has same NotAfter value: saw %s, expected greather than %s", newCert.NotAfter, cert.NotAfter)
			}

			certtestutil.AssertCertificateIsSignedByCa(t, newCert, testCACert)
			certtestutil.AssertCertificateHasClientAuthUsage(t, newCert)
			certtestutil.AssertCertificateHasOrganizations(t, newCert, test.expectedOrganization...)
			certtestutil.AssertCertificateHasCommonName(t, newCert, testCertCfg.CommonName)
			certtestutil.AssertCertificateHasDNSNames(t, newCert, testCertCfg.AltNames.DNSNames...)
			certtestutil.AssertCertificateHasIPAddresses(t, newCert, testCertCfg.AltNames.IPs...)
		})
	}
}

func TestCreateRenewCSR(t *testing.T) {
	dir := testutil.SetupTempDir(t)
	defer os.RemoveAll(dir)

	outdir := filepath.Join(dir, "out")

	if err := os.MkdirAll(outdir, 0755); err != nil {
		t.Fatalf("couldn't create %s", outdir)
	}

	if err := pkiutil.WriteCertAndKey(dir, "ca", testCACert, testCAKey); err != nil {
		t.Fatalf("couldn't write out CA certificate to %s", dir)
	}

	cfg := &kubeadmapi.ClusterConfiguration{
		CertificatesDir: dir,
	}
	rm, err := NewManager(cfg, dir)
	if err != nil {
		t.Fatalf("Failed to create the certificate renewal manager: %v", err)
	}

	tests := []struct {
		name           string
		certName       string
		createCertFunc func() *x509.Certificate
	}{
		{
			name:     "Creation of a CSR request for renewal of a PKI certificate",
			certName: "apiserver",
			createCertFunc: func() *x509.Certificate {
				return writeTestCertificate(t, dir, "apiserver", testCACert, testCAKey, testCertOrganization)
			},
		},
		{
			name:     "Creation of a CSR request for renewal of a certificate embedded in a kubeconfig file",
			certName: "admin.conf",
			createCertFunc: func() *x509.Certificate {
				return writeTestKubeconfig(t, dir, "admin.conf", testCACert, testCAKey)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.createCertFunc()

			time.Sleep(1 * time.Second)

			err := rm.CreateRenewCSR(test.certName, outdir)
			if err != nil {
				t.Fatalf("error renewing certificate: %v", err)
			}

			file := fmt.Sprintf("%s.key", test.certName)
			if _, err := os.Stat(filepath.Join(outdir, file)); os.IsNotExist(err) {
				t.Errorf("Expected file %s does not exist", file)
			}

			file = fmt.Sprintf("%s.csr", test.certName)
			if _, err := os.Stat(filepath.Join(outdir, file)); os.IsNotExist(err) {
				t.Errorf("Expected file %s does not exist", file)
			}
		})
	}

}

func TestCertToConfig(t *testing.T) {
	expectedConfig := &certutil.Config{
		CommonName:   "test-common-name",
		Organization: testCertOrganization,
		AltNames: certutil.AltNames{
			IPs:      []net.IP{netutils.ParseIPSloppy("10.100.0.1")},
			DNSNames: []string{"test-domain.space"},
		},
		Usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	cert := &x509.Certificate{
		Subject: pkix.Name{
			CommonName:   "test-common-name",
			Organization: testCertOrganization,
		},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{"test-domain.space"},
		IPAddresses: []net.IP{netutils.ParseIPSloppy("10.100.0.1")},
	}

	cfg := certToConfig(cert)

	if cfg.CommonName != expectedConfig.CommonName {
		t.Errorf("expected common name %q, got %q", expectedConfig.CommonName, cfg.CommonName)
	}

	if len(cfg.Organization) != 1 || cfg.Organization[0] != expectedConfig.Organization[0] {
		t.Errorf("expected organization %v, got %v", expectedConfig.Organization, cfg.Organization)

	}

	if len(cfg.Usages) != 1 || cfg.Usages[0] != expectedConfig.Usages[0] {
		t.Errorf("expected ext key usage %v, got %v", expectedConfig.Usages, cfg.Usages)
	}

	if len(cfg.AltNames.IPs) != 1 || cfg.AltNames.IPs[0].String() != expectedConfig.AltNames.IPs[0].String() {
		t.Errorf("expected SAN IPs %v, got %v", expectedConfig.AltNames.IPs, cfg.AltNames.IPs)
	}

	if len(cfg.AltNames.DNSNames) != 1 || cfg.AltNames.DNSNames[0] != expectedConfig.AltNames.DNSNames[0] {
		t.Errorf("expected SAN DNSNames %v, got %v", expectedConfig.AltNames.DNSNames, cfg.AltNames.DNSNames)
	}
}

func makeTestCertConfig(organization []string) *pkiutil.CertConfig {
	return &pkiutil.CertConfig{
		Config: certutil.Config{
			CommonName:   "test-common-name",
			Organization: organization,
			AltNames: certutil.AltNames{
				IPs:      []net.IP{netutils.ParseIPSloppy("10.100.0.1")},
				DNSNames: []string{"test-domain.space"},
			},
			Usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
	}
}

func TestManagerCAs(t *testing.T) {
	tests := []struct {
		name string
		cas  map[string]*CAExpirationHandler
		want []*CAExpirationHandler
	}{
		{
			name: "CAExpirationHandler is sequential",
			cas: map[string]*CAExpirationHandler{
				"foo": {
					Name: "1",
				},
				"bar": {
					Name: "2",
				},
			},
			want: []*CAExpirationHandler{
				{
					Name: "1",
				},
				{
					Name: "2",
				},
			},
		},
		{
			name: "CAExpirationHandler is in reverse order",
			cas: map[string]*CAExpirationHandler{
				"foo": {
					Name: "2",
				},
				"bar": {
					Name: "1",
				},
			},
			want: []*CAExpirationHandler{
				{
					Name: "1",
				},
				{
					Name: "2",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rm := &Manager{
				cas: tt.cas,
			}
			if got := rm.CAs(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Manager.CAs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagerCAExists(t *testing.T) {
	certificateReadWriterExist := fakecertificateReadWriter{
		exist: true,
	}
	certificateReadWriterMissing := fakecertificateReadWriter{
		exist: false,
	}
	tests := []struct {
		name    string
		cas     map[string]*CAExpirationHandler
		caName  string
		want    bool
		wantErr bool
	}{
		{
			name:    "caName does not exist in cas list",
			cas:     map[string]*CAExpirationHandler{},
			caName:  "foo",
			want:    false,
			wantErr: true,
		},
		{
			name: "ca exists",
			cas: map[string]*CAExpirationHandler{
				"foo": {
					Name:       "foo",
					FileName:   "test",
					readwriter: certificateReadWriterExist,
				},
			},
			caName:  "foo",
			want:    true,
			wantErr: false,
		},
		{
			name: "ca does not exist",
			cas: map[string]*CAExpirationHandler{
				"foo": {
					Name:       "foo",
					FileName:   "test",
					readwriter: certificateReadWriterMissing,
				},
			},
			caName:  "foo",
			want:    false,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rm := &Manager{
				cas: tt.cas,
			}
			got, err := rm.CAExists(tt.caName)
			if (err != nil) != tt.wantErr {
				t.Errorf("Manager.CAExists() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Manager.CAExists() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagerCertificateExists(t *testing.T) {
	certificateReadWriterExist := fakecertificateReadWriter{
		exist: true,
	}
	certificateReadWriterMissing := fakecertificateReadWriter{
		exist: false,
	}
	tests := []struct {
		name         string
		certificates map[string]*CertificateRenewHandler
		certName     string
		want         bool
		wantErr      bool
	}{
		{
			name:         "certName does not exist in certificate list",
			certificates: map[string]*CertificateRenewHandler{},
			certName:     "foo",
			want:         false,
			wantErr:      true,
		},
		{
			name: "certificate exists",
			certificates: map[string]*CertificateRenewHandler{
				"foo": {
					Name:       "foo",
					readwriter: certificateReadWriterExist,
				},
			},
			certName: "foo",
			want:     true,
			wantErr:  false,
		},
		{
			name: "certificate does not exist",
			certificates: map[string]*CertificateRenewHandler{
				"foo": {
					Name:       "foo",
					readwriter: certificateReadWriterMissing,
				},
			},
			certName: "foo",
			want:     false,
			wantErr:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rm := &Manager{
				certificates: tt.certificates,
			}
			got, err := rm.CertificateExists(tt.certName)
			if (err != nil) != tt.wantErr {
				t.Errorf("Manager.CertificateExists() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Manager.CertificateExists() = %v, want %v", got, tt.want)
			}
		})
	}
}
