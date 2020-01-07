package handler

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/kubeapps/kubeapps/pkg/agent"
	"github.com/kubeapps/kubeapps/pkg/auth"
	authFake "github.com/kubeapps/kubeapps/pkg/auth/fake"
	chartFake "github.com/kubeapps/kubeapps/pkg/chart/fake"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"

	"helm.sh/helm/v3/pkg/release"
)

const defaultListLimit = 256

// newConfigFixture returns an agent.Config with fake clients
// and memory storage.
func newConfigFixture(t *testing.T) *agent.Config {
	t.Helper()

	return &agent.Config{
		ActionConfig: &action.Configuration{
			Releases:     storage.Init(driver.NewMemory()),
			KubeClient:   &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: ioutil.Discard}},
			Capabilities: chartutil.DefaultCapabilities,
			Log: func(format string, v ...interface{}) {
				t.Helper()
				t.Logf(format, v...)
			},
		},
		ChartClient: &chartFake.FakeChart{},
		AgentOptions: agent.Options{
			ListLimit: defaultListLimit,
		},
	}
}

func TestActions(t *testing.T) {
	type testScenario struct {
		// Scenario params
		Description      string
		ExistingReleases []release.Release
		DisableAuth      bool
		// Request params
		RequestBody  string
		RequestQuery string
		Action       string
		Params       map[string]string
		// Expected result
		StatusCode        int
		RemainingReleases []release.Release
		ResponseBody      string //optional
	}

	tests := []testScenario{
		{
			// Scenario params
			Description: "List all releases",
			ExistingReleases: []release.Release{
				createRelease("ch1", "foobar", "default", 1, release.StatusDeployed),
				createRelease("ch2", "foo", "not-default", 1, release.StatusDeployed),
			},
			DisableAuth: false,
			// Request params
			RequestBody:  "",
			RequestQuery: "",
			Action:       "listall",
			Params:       map[string]string{},
			// Expected result
			StatusCode: 200,
			RemainingReleases: []release.Release{
				createRelease("ch1", "foobar", "default", 1, release.StatusDeployed),
				createRelease("ch2", "foo", "not-default", 1, release.StatusDeployed),
			},
			ResponseBody: `{"data":[{"releaseName":"foo","version":"1","namespace":"not-default","status":"deployed","chart":"","chartMetadata":{}},{"releaseName":"foobar","version":"1","namespace":"default","status":"deployed","chart":"","chartMetadata":{}}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.Description, func(t *testing.T) {
			// Initialize environment for test
			req := httptest.NewRequest("GET", fmt.Sprintf("http://foo.bar%s", test.RequestQuery), strings.NewReader(test.RequestBody))
			if !test.DisableAuth {
				fauth := &authFake.FakeAuth{}
				ctx := context.WithValue(req.Context(), auth.UserKey, fauth)
				req = req.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			cfg := newConfigFixture(t)
			for i := range test.ExistingReleases {
				err := cfg.ActionConfig.Releases.Create(&test.ExistingReleases[i])
				if err != nil {
					t.Errorf("Failed to initiate test: %v", err)
				}
			}
			// Perform request
			switch test.Action {
			case "listall":
				ListAllReleases(*cfg, response, req, test.Params)
			default:
				t.Errorf("Unexpected action %s", test.Action)
			}
			// Check result
			if response.Code != test.StatusCode {
				t.Errorf("Expecting a StatusCode %d, received %d", test.StatusCode, response.Code)
			}
			releases := derefReleases(cfg.ActionConfig.Releases)
			rlsComparer := cmp.Comparer(func(x release.Release, y release.Release) bool {
				return x.Name == y.Name &&
					x.Version == y.Version &&
					x.Namespace == y.Namespace &&
					x.Info.Status == y.Info.Status &&
					x.Chart.Name() == y.Chart.Name() &&
					x.Manifest == y.Manifest &&
					cmp.Equal(x.Config, y.Config) &&
					cmp.Equal(x.Hooks, y.Hooks)
			})
			if !cmp.Equal(releases, test.RemainingReleases, rlsComparer) {
				t.Errorf("Unexpected remaining releases. Diff %s", cmp.Diff(releases, test.RemainingReleases, rlsComparer))
			}
			if test.ResponseBody != "" {
				if test.ResponseBody != response.Body.String() {
					t.Errorf("Unexpected body response. Diff %s", cmp.Diff(test.ResponseBody, response.Body))
				}
			}
		})
	}
}

// derefReleases derefrences the releases in sotrage into an array
func derefReleases(storage *storage.Storage) []release.Release {
	rls, _ := storage.ListReleases()
	releases := make([]release.Release, len(rls))
	for i := range rls {
		releases[i] = *rls[i]
	}
	return releases
}

func createRelease(chartName, name, namespace string, version int, status release.Status) release.Release {
	return release.Release{
		Name:      name,
		Namespace: namespace,
		Version:   version,
		Info:      &release.Info{Status: status},
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{
				Name: chartName,
			},
			Values: make(map[string]interface{}),
		},
		Config: make(map[string]interface{}),
	}
}
