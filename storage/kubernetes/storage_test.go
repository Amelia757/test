package kubernetes

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/suite"
	"sigs.k8s.io/testing_frameworks/integration"

	"github.com/concourse/dex/storage"
	"github.com/concourse/dex/storage/conformance"
)

const kubeconfigTemplate = `apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    server: SERVERURL
users:
- name: local
  user:
contexts:
- context:
    cluster: local
    user: local
`

func TestStorage(t *testing.T) {
	if os.Getenv("TEST_ASSET_KUBE_APISERVER") == "" || os.Getenv("TEST_ASSET_ETCD") == "" {
		t.Skip("control plane binaries are missing")
	}

	suite.Run(t, new(StorageTestSuite))
}

type StorageTestSuite struct {
	suite.Suite

	controlPlane *integration.ControlPlane

	client *client
}

func (s *StorageTestSuite) SetupSuite() {
	s.controlPlane = &integration.ControlPlane{}

	err := s.controlPlane.Start()
	s.Require().NoError(err)
}

func (s *StorageTestSuite) TearDownSuite() {
	s.controlPlane.Stop()
}

func (s *StorageTestSuite) SetupTest() {
	f, err := ioutil.TempFile("", "dex-kubeconfig-*")
	s.Require().NoError(err)
	defer f.Close()

	_, err = f.WriteString(strings.ReplaceAll(kubeconfigTemplate, "SERVERURL", s.controlPlane.APIURL().String()))
	s.Require().NoError(err)

	config := Config{
		KubeConfigFile: f.Name(),
	}

	logger := &logrus.Logger{
		Out:       os.Stderr,
		Formatter: &logrus.TextFormatter{DisableColors: true},
		Level:     logrus.DebugLevel,
	}

	client, err := config.open(logger, true)
	s.Require().NoError(err)

	s.client = client
}

func (s *StorageTestSuite) TestStorage() {
	newStorage := func() storage.Storage {
		for _, resource := range []string{
			resourceAuthCode,
			resourceAuthRequest,
			resourceDeviceRequest,
			resourceDeviceToken,
			resourceClient,
			resourceRefreshToken,
			resourceKeys,
			resourcePassword,
		} {
			if err := s.client.deleteAll(resource); err != nil {
				s.T().Fatalf("delete all %q failed: %v", resource, err)
			}
		}
		return s.client
	}

	conformance.RunTests(s.T(), newStorage)
	conformance.RunTransactionTests(s.T(), newStorage)
}

func TestURLFor(t *testing.T) {
	tests := []struct {
		apiVersion, namespace, resource, name string

		baseURL string
		want    string
	}{
		{
			"v1", "default", "pods", "a",
			"https://k8s.example.com",
			"https://k8s.example.com/api/v1/namespaces/default/pods/a",
		},
		{
			"foo/v1", "default", "bar", "a",
			"https://k8s.example.com",
			"https://k8s.example.com/apis/foo/v1/namespaces/default/bar/a",
		},
		{
			"foo/v1", "default", "bar", "a",
			"https://k8s.example.com/",
			"https://k8s.example.com/apis/foo/v1/namespaces/default/bar/a",
		},
		{
			"foo/v1", "default", "bar", "a",
			"https://k8s.example.com/",
			"https://k8s.example.com/apis/foo/v1/namespaces/default/bar/a",
		},
		{
			// no namespace
			"foo/v1", "", "bar", "a",
			"https://k8s.example.com",
			"https://k8s.example.com/apis/foo/v1/bar/a",
		},
	}

	for _, test := range tests {
		c := &client{baseURL: test.baseURL}
		got := c.urlFor(test.apiVersion, test.namespace, test.resource, test.name)
		if got != test.want {
			t.Errorf("(&client{baseURL:%q}).urlFor(%q, %q, %q, %q): expected %q got %q",
				test.baseURL,
				test.apiVersion, test.namespace, test.resource, test.name,
				test.want, got,
			)
		}
	}
}

func TestUpdateKeys(t *testing.T) {
	fakeUpdater := func(old storage.Keys) (storage.Keys, error) { return storage.Keys{}, nil }

	tests := []struct {
		name               string
		updater            func(old storage.Keys) (storage.Keys, error)
		getResponseCode    int
		actionResponseCode int
		wantErr            bool
		exactErr           error
	}{
		{
			"Create OK test",
			fakeUpdater,
			404,
			201,
			false,
			nil,
		},
		{
			"Update should be OK",
			fakeUpdater,
			200,
			200,
			false,
			nil,
		},
		{
			"Create conflict should be OK",
			fakeUpdater,
			404,
			409,
			true,
			errors.New("keys already created by another server instance"),
		},
		{
			"Update conflict should be OK",
			fakeUpdater,
			200,
			409,
			true,
			errors.New("keys already rotated by another server instance"),
		},
		{
			"Client error is error",
			fakeUpdater,
			404,
			500,
			true,
			nil,
		},
		{
			"Client error during update is error",
			fakeUpdater,
			200,
			500,
			true,
			nil,
		},
		{
			"Get error is error",
			fakeUpdater,
			500,
			200,
			true,
			nil,
		},
		{
			"Updater error is error",
			func(old storage.Keys) (storage.Keys, error) { return storage.Keys{}, fmt.Errorf("test") },
			200,
			201,
			true,
			nil,
		},
	}

	for _, test := range tests {
		client := newStatusCodesResponseTestClient(test.getResponseCode, test.actionResponseCode)

		err := client.UpdateKeys(test.updater)
		if err != nil {
			if !test.wantErr {
				t.Fatalf("Test %q: %v", test.name, err)
			}

			if test.exactErr != nil && test.exactErr.Error() != err.Error() {
				t.Fatalf("Test %q: %v, wanted: %v", test.name, err, test.exactErr)
			}
		}
	}
}

func newStatusCodesResponseTestClient(getResponseCode, actionResponseCode int) *client {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(getResponseCode)
		} else {
			w.WriteHeader(actionResponseCode)
		}
		w.Write([]byte(`{}`)) // Empty json is enough, we will test only response codes here
	}))

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &client{
		client:  &http.Client{Transport: tr},
		baseURL: s.URL,
		logger: &logrus.Logger{
			Out:       os.Stderr,
			Formatter: &logrus.TextFormatter{DisableColors: true},
			Level:     logrus.DebugLevel,
		},
	}
}
