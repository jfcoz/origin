// +build integration

package integration

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	ktestclient "k8s.io/kubernetes/pkg/client/unversioned/testclient"
	"k8s.io/kubernetes/pkg/kubelet/dockertools"
	"k8s.io/kubernetes/pkg/runtime"
	utilerrs "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/sets"

	buildapi "github.com/openshift/origin/pkg/build/api"
	client "github.com/openshift/origin/pkg/client/testclient"
	deployapi "github.com/openshift/origin/pkg/deploy/api"
	"github.com/openshift/origin/pkg/dockerregistry"
	"github.com/openshift/origin/pkg/generate/app"
	"github.com/openshift/origin/pkg/generate/app/cmd"
	"github.com/openshift/origin/pkg/generate/dockerfile"
	"github.com/openshift/origin/pkg/generate/source"
	imageapi "github.com/openshift/origin/pkg/image/api"
	templateapi "github.com/openshift/origin/pkg/template/api"

	_ "github.com/openshift/origin/pkg/api/install"
)

func skipExternalGit(t *testing.T) {
	if len(os.Getenv("SKIP_EXTERNAL_GIT")) > 0 {
		t.Skip("external Git tests are disabled")
	}
}

func TestNewAppAddArguments(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "test-newapp")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	testDir := filepath.Join(tmpDir, "test/one/two/three")
	err = os.MkdirAll(testDir, 0777)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	tests := map[string]struct {
		args       []string
		env        []string
		parms      []string
		repos      []string
		components []string
		unknown    []string
	}{
		"components": {
			args:       []string{"one", "two+three", "four~five"},
			components: []string{"one", "two+three", "four~five"},
			unknown:    []string{},
		},
		"source": {
			args:    []string{".", testDir, "git://github.com/openshift/origin.git"},
			repos:   []string{".", testDir, "git://github.com/openshift/origin.git"},
			unknown: []string{},
		},
		"source custom ref": {
			args:    []string{"https://github.com/openshift/ruby-hello-world#beta4"},
			repos:   []string{"https://github.com/openshift/ruby-hello-world#beta4"},
			unknown: []string{},
		},
		"env": {
			args:    []string{"first=one", "second=two", "third=three"},
			env:     []string{"first=one", "second=two", "third=three"},
			unknown: []string{},
		},
		"mix 1": {
			args:       []string{"git://github.com/openshift/origin.git", "mysql+ruby~git@github.com/openshift/origin.git", "env1=test", "ruby-helloworld-sample"},
			repos:      []string{"git://github.com/openshift/origin.git"},
			components: []string{"mysql+ruby~git@github.com/openshift/origin.git", "ruby-helloworld-sample"},
			env:        []string{"env1=test"},
			unknown:    []string{},
		},
	}

	for n, c := range tests {
		a := cmd.AppConfig{}
		unknown := a.AddArguments(c.args)
		if !reflect.DeepEqual(a.Environment, c.env) {
			t.Errorf("%s: Different env variables. Expected: %v, Actual: %v", n, c.env, a.Environment)
		}
		if !reflect.DeepEqual(a.SourceRepositories, c.repos) {
			t.Errorf("%s: Different source repos. Expected: %v, Actual: %v", n, c.repos, a.SourceRepositories)
		}
		if !reflect.DeepEqual(a.Components, c.components) {
			t.Errorf("%s: Different components. Expected: %v, Actual: %v", n, c.components, a.Components)
		}
		if !reflect.DeepEqual(unknown, c.unknown) {
			t.Errorf("%s: Different unknown result. Expected: %v, Actual: %v", n, c.unknown, unknown)
		}
	}

}

func TestNewAppResolve(t *testing.T) {
	tests := []struct {
		name        string
		cfg         cmd.AppConfig
		components  app.ComponentReferences
		expectedErr string
	}{
		{
			name: "Resolver error",
			components: app.ComponentReferences{
				app.ComponentReference(&app.ComponentInput{
					Value: "mysql:invalid",
					Resolver: app.UniqueExactOrInexactMatchResolver{
						Searcher: app.DockerRegistrySearcher{
							Client: dockerregistry.NewClient(10*time.Second, true),
						},
					},
				})},
			expectedErr: `no match for "mysql:invalid`,
		},
		{
			name: "Successful mysql builder",
			components: app.ComponentReferences{
				app.ComponentReference(&app.ComponentInput{
					Value: "mysql",
					ResolvedMatch: &app.ComponentMatch{
						Builder: true,
					},
				})},
			expectedErr: "",
		},
		{
			name: "Unable to build source code",
			components: app.ComponentReferences{
				app.ComponentReference(&app.ComponentInput{
					Value:         "mysql",
					ExpectToBuild: true,
				})},
			expectedErr: "no resolver",
		},
		{
			name: "Successful docker build",
			cfg: cmd.AppConfig{
				Strategy: "docker",
			},
			components: app.ComponentReferences{
				app.ComponentReference(&app.ComponentInput{
					Value:         "mysql",
					ExpectToBuild: true,
				})},
			expectedErr: "",
		},
	}

	for _, test := range tests {
		err := cmd.Resolve(test.components)
		if err != nil {
			if !strings.Contains(err.Error(), test.expectedErr) {
				t.Errorf("%s: Invalid error: Expected %s, got %v", test.name, test.expectedErr, err)
			}
		} else if len(test.expectedErr) != 0 {
			t.Errorf("%s: Expected %s error but got none", test.name, test.expectedErr)
		}
	}
}

func TestNewAppDetectSource(t *testing.T) {
	skipExternalGit(t)
	gitLocalDir := createLocalGitDirectory(t)
	defer os.RemoveAll(gitLocalDir)

	dockerSearcher := app.DockerRegistrySearcher{
		Client: dockerregistry.NewClient(10*time.Second, true),
	}
	mocks := MockSourceRepositories(t, gitLocalDir)
	tests := []struct {
		name         string
		cfg          *cmd.AppConfig
		repositories []*app.SourceRepository
		expectedLang string
		expectedErr  string
	}{
		{
			name: "detect source - ruby",
			cfg: &cmd.AppConfig{
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				DockerSearcher: dockerSearcher,
			},
			repositories: []*app.SourceRepository{mocks[0]},
			expectedLang: "ruby",
			expectedErr:  "",
		},
	}

	for _, test := range tests {
		err := test.cfg.DetectSource(test.repositories)
		if err != nil {
			if !strings.Contains(err.Error(), test.expectedErr) {
				t.Errorf("%s: Invalid error: Expected %s, got %v", test.name, test.expectedErr, err)
			}
		} else if len(test.expectedErr) != 0 {
			t.Errorf("%s: Expected %s error but got none", test.name, test.expectedErr)
		}

		for _, repo := range test.repositories {
			info := repo.Info()
			if info == nil {
				t.Errorf("%s: expected repository info to be populated; it is nil", test.name)
				continue
			}
			if term := strings.Join(info.Terms(), ","); term != test.expectedLang {
				t.Errorf("%s: expected repository info term to be %s; got %s\n", test.name, test.expectedLang, term)
			}
		}
	}
}

func mapContains(a, b map[string]string) bool {
	for k, v := range a {
		if v2, exists := b[k]; !exists || v != v2 {
			return false
		}
	}
	return true
}

// ExactMatchDockerSearcher returns a match with the value that was passed in
// and a march score of 0.0(exact)
type ExactMatchDockerSearcher struct {
	Errs []error
}

// Search always returns a match for every term passed in
func (r *ExactMatchDockerSearcher) Search(precise bool, terms ...string) (app.ComponentMatches, []error) {
	matches := app.ComponentMatches{}
	for _, value := range terms {
		matches = append(matches, &app.ComponentMatch{
			Value:       value,
			Name:        value,
			Argument:    fmt.Sprintf("--docker-image=%q", value),
			Description: fmt.Sprintf("Docker image %q", value),
			Score:       0.0,
		})
	}
	return matches, r.Errs
}

func TestNewAppRunAll(t *testing.T) {
	skipExternalGit(t)
	dockerSearcher := app.DockerRegistrySearcher{
		Client: dockerregistry.NewClient(10*time.Second, true),
	}
	tests := []struct {
		name            string
		config          *cmd.AppConfig
		expected        map[string][]string
		expectedName    string
		expectedErr     error
		errFn           func(error) bool
		expectInsecure  sets.String
		expectedVolumes map[string]string
		checkPort       string
	}{
		{
			name: "successful ruby app generation",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},

				DockerSearcher: fakeDockerSearcher(),
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				Strategy:                        "source",
				ImageStreamByAnnotationSearcher: app.NewImageStreamByAnnotationSearcher(&client.Fake{}, &client.Fake{}, []string{"default"}),
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},
			expected: map[string][]string{
				"imageStream":      {"ruby-hello-world", "ruby"},
				"buildConfig":      {"ruby-hello-world"},
				"deploymentConfig": {"ruby-hello-world"},
				"service":          {"ruby-hello-world"},
			},
			expectedName:    "ruby-hello-world",
			expectedVolumes: nil,
			expectedErr:     nil,
		},
		{
			name: "successful ruby app generation with labels",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},

				DockerSearcher: fakeDockerSearcher(),
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				Strategy:                        "source",
				ImageStreamByAnnotationSearcher: app.NewImageStreamByAnnotationSearcher(&client.Fake{}, &client.Fake{}, []string{"default"}),
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
				Labels:          map[string]string{"label1": "value1", "label2": "value2"},
			},
			expected: map[string][]string{
				"imageStream":      {"ruby-hello-world", "ruby"},
				"buildConfig":      {"ruby-hello-world"},
				"deploymentConfig": {"ruby-hello-world"},
				"service":          {"ruby-hello-world"},
			},
			expectedName:    "ruby-hello-world",
			expectedVolumes: nil,
			expectedErr:     nil,
		},
		{
			name: "successful docker app generation",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},

				DockerSearcher: fakeSimpleDockerSearcher(),
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				Strategy:                        "docker",
				ImageStreamByAnnotationSearcher: app.NewImageStreamByAnnotationSearcher(&client.Fake{}, &client.Fake{}, []string{"default"}),
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},
			checkPort: "8080",
			expected: map[string][]string{
				"imageStream":      {"ruby-hello-world", "ruby-22-centos7"},
				"buildConfig":      {"ruby-hello-world"},
				"deploymentConfig": {"ruby-hello-world"},
				"service":          {"ruby-hello-world"},
			},
			expectedName: "ruby-hello-world",
			expectedErr:  nil,
		},
		{
			name: "app generation using context dir",
			config: &cmd.AppConfig{
				SourceRepositories:              []string{"https://github.com/openshift/sti-ruby"},
				ContextDir:                      "2.0/test/rack-test-app",
				DockerSearcher:                  dockerSearcher,
				ImageStreamSearcher:             fakeImageStreamSearcher(),
				ImageStreamByAnnotationSearcher: app.NewImageStreamByAnnotationSearcher(&client.Fake{}, &client.Fake{}, []string{"default"}),
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},

				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},
			expected: map[string][]string{
				"imageStream":      {"sti-ruby"},
				"buildConfig":      {"sti-ruby"},
				"deploymentConfig": {"sti-ruby"},
				"service":          {"sti-ruby"},
			},
			expectedName:    "sti-ruby",
			expectedVolumes: nil,
			expectedErr:     nil,
		},
		{
			name: "insecure registry generation",
			config: &cmd.AppConfig{
				Components:         []string{"myrepo:5000/myco/example"},
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},
				Strategy:           "source",
				DockerSearcher: app.DockerClientSearcher{
					Client: &dockertools.FakeDockerClient{
						Images: []docker.APIImages{{RepoTags: []string{"myrepo:5000/myco/example"}}},
						Image:  dockerBuilderImage(),
					},
					Insecure:         true,
					RegistrySearcher: &ExactMatchDockerSearcher{},
				},
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{},
				},
				TemplateFileSearcher: &app.TemplateFileSearcher{},
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:            kapi.Scheme,
				OSClient:         &client.Fake{},
				OriginNamespace:  "default",
				InsecureRegistry: true,
			},
			expected: map[string][]string{
				"imageStream":      {"example", "ruby-hello-world"},
				"buildConfig":      {"ruby-hello-world"},
				"deploymentConfig": {"ruby-hello-world"},
				"service":          {"ruby-hello-world"},
			},
			expectedName:    "ruby-hello-world",
			expectedErr:     nil,
			expectedVolumes: nil,
			expectInsecure:  sets.NewString("example"),
		},
		{
			name: "emptyDir volumes",
			config: &cmd.AppConfig{
				DockerImages: []string{"mysql"},

				DockerSearcher: dockerSearcher,
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},

				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},

			expected: map[string][]string{
				"imageStream":      {"mysql"},
				"deploymentConfig": {"mysql"},
				"service":          {"mysql"},
				"volumeMounts":     {"mysql-volume-1"},
			},
			expectedName: "mysql",
			expectedVolumes: map[string]string{
				"mysql-volume-1": "EmptyDir",
			},
			expectedErr: nil,
		},
		{
			name: "Docker build",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},

				DockerSearcher: app.DockerClientSearcher{
					Client: &dockertools.FakeDockerClient{
						Images: []docker.APIImages{{RepoTags: []string{"centos/ruby-22-centos7"}}},
						Image:  dockerBuilderImage(),
					},
					Insecure:         true,
					RegistrySearcher: &ExactMatchDockerSearcher{},
				},
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				ImageStreamByAnnotationSearcher: app.NewImageStreamByAnnotationSearcher(&client.Fake{}, &client.Fake{}, []string{"default"}),
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},
			expected: map[string][]string{
				"imageStream":      {"ruby-hello-world", "ruby-22-centos7"},
				"buildConfig":      {"ruby-hello-world"},
				"deploymentConfig": {"ruby-hello-world"},
				"service":          {"ruby-hello-world"},
			},
			expectedName: "ruby-hello-world",
			expectedErr:  nil,
		},
		{
			name: "Docker build with no registry image",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},

				DockerSearcher: app.DockerClientSearcher{
					Client: &dockertools.FakeDockerClient{
						Images: []docker.APIImages{{RepoTags: []string{"centos/ruby-22-centos7"}}},
						Image:  dockerBuilderImage(),
					},
					Insecure: true,
				},
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				ImageStreamByAnnotationSearcher: app.NewImageStreamByAnnotationSearcher(&client.Fake{}, &client.Fake{}, []string{"default"}),
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},
			expected: map[string][]string{
				"imageStream":      {"ruby-hello-world"},
				"buildConfig":      {"ruby-hello-world"},
				"deploymentConfig": {"ruby-hello-world"},
				"service":          {"ruby-hello-world"},
			},
			expectedName: "ruby-hello-world",
			expectedErr:  nil,
		},
		{
			name: "custom name",
			config: &cmd.AppConfig{
				DockerImages: []string{"mysql"},
				DockerSearcher: app.DockerClientSearcher{
					Client: &dockertools.FakeDockerClient{
						Images: []docker.APIImages{{RepoTags: []string{"mysql"}}},
						Image: &docker.Image{
							Config: &docker.Config{
								ExposedPorts: map[docker.Port]struct{}{
									"8080/tcp": {},
								},
							},
						},
					},
					RegistrySearcher: &ExactMatchDockerSearcher{},
				},
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client:            &client.Fake{},
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
				Name:            "custom",
			},
			expected: map[string][]string{
				"imageStream":      {"custom"},
				"deploymentConfig": {"custom"},
				"service":          {"custom"},
			},
			expectedName: "custom",
			expectedErr:  nil,
		},
		{
			name: "partial matches",
			config: &cmd.AppConfig{
				DockerImages: []string{"mysql"},
				DockerSearcher: app.DockerClientSearcher{
					RegistrySearcher: &ExactMatchDockerSearcher{Errs: []error{errors.NewInternalError(fmt.Errorf("test error"))}},
				},
				ImageStreamSearcher: app.ImageStreamSearcher{
					Client: client.NewSimpleFake(&unversioned.Status{
						Status: unversioned.StatusFailure,
						Code:   http.StatusInternalServerError,
						Reason: unversioned.StatusReasonInternalError,
					}),
					ImageStreamImages: &client.Fake{},
					Namespaces:        []string{"default"},
				},
				TemplateSearcher: app.TemplateSearcher{
					Client: &client.Fake{},
					TemplateConfigsNamespacer: &client.Fake{},
					Namespaces:                []string{"openshift", "default"},
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
				Name:            "custom",
			},
			expected: map[string][]string{
				"imageStream":      {"custom"},
				"deploymentConfig": {"custom"},
				"service":          {"custom"},
			},
			expectedName: "custom",
			errFn: func(err error) bool {
				err = err.(utilerrs.Aggregate).Errors()[0]
				match, ok := err.(app.ErrNoMatch)
				if !ok {
					return false
				}
				if match.Value != "mysql" {
					return false
				}
				t.Logf("%#v", match.Errs[0])
				return len(match.Errs) == 1 && strings.Contains(match.Errs[0].Error(), "test error")
			},
		},
	}

	for _, test := range tests {
		test.config.RefBuilder = &app.ReferenceBuilder{}
		test.config.Out, test.config.ErrOut = os.Stdout, os.Stderr
		test.config.Deploy = true
		res, err := test.config.Run()
		if test.errFn != nil {
			if !test.errFn(err) {
				t.Errorf("%s: Error mismatch! Unexpected error: %#v", test.name, err)
				continue
			}
		} else if err != test.expectedErr {
			t.Errorf("%s: Error mismatch! Expected %v, got %v", test.name, test.expectedErr, err)
			continue
		}
		if err != nil {
			continue
		}
		if res.Name != test.expectedName {
			t.Errorf("%s: Name was not correct: %v", test.name, res.Name)
			continue
		}
		imageStreams := []*imageapi.ImageStream{}
		got := map[string][]string{}
		gotVolumes := map[string]string{}
		for _, obj := range res.List.Items {
			switch tp := obj.(type) {
			case *buildapi.BuildConfig:
				got["buildConfig"] = append(got["buildConfig"], tp.Name)
			case *kapi.Service:
				if test.checkPort != "" {
					if len(tp.Spec.Ports) == 0 {
						t.Errorf("%s: did not get any ports in service", test.name)
						break
					}
					expectedPort, _ := strconv.Atoi(test.checkPort)
					if tp.Spec.Ports[0].Port != expectedPort {
						t.Errorf("%s: did not get expected port in service. Expected: %d. Got %d\n",
							test.name, expectedPort, tp.Spec.Ports[0].Port)
					}
				}
				if test.config.Labels != nil {
					if !mapContains(test.config.Labels, tp.Spec.Selector) {
						t.Errorf("%s: did not get expected service selector. Expected: %v. Got: %v",
							test.name, test.config.Labels, tp.Spec.Selector)
					}
				}
				got["service"] = append(got["service"], tp.Name)
			case *imageapi.ImageStream:
				got["imageStream"] = append(got["imageStream"], tp.Name)
				imageStreams = append(imageStreams, tp)
			case *deployapi.DeploymentConfig:
				got["deploymentConfig"] = append(got["deploymentConfig"], tp.Name)
				if podTemplate := tp.Spec.Template; podTemplate != nil {
					for _, volume := range podTemplate.Spec.Volumes {
						if volume.VolumeSource.EmptyDir != nil {
							gotVolumes[volume.Name] = "EmptyDir"
						} else {
							gotVolumes[volume.Name] = "UNKNOWN"
						}
					}
					for _, container := range podTemplate.Spec.Containers {
						for _, volumeMount := range container.VolumeMounts {
							got["volumeMounts"] = append(got["volumeMounts"], volumeMount.Name)
						}
					}
				}
				if test.config.Labels != nil {
					if !mapContains(test.config.Labels, tp.Spec.Selector) {
						t.Errorf("%s: did not get expected deployment config rc selector. Expected: %v. Got: %v",
							test.name, test.config.Labels, tp.Spec.Selector)
					}
				}
			}
		}

		if len(test.expected) != len(got) {
			t.Errorf("%s: Resource kind size mismatch! Expected %d, got %d", test.name, len(test.expected), len(got))
			continue
		}
		for k, exp := range test.expected {
			g, ok := got[k]
			if !ok {
				t.Errorf("%s: Didn't find expected kind %s", test.name, k)
			}

			sort.Strings(g)
			sort.Strings(exp)

			if !reflect.DeepEqual(g, exp) {
				t.Errorf("%s: %s resource names mismatch! Expected %v, got %v", test.name, k, exp, g)
				continue
			}
		}

		if len(test.expectedVolumes) != len(gotVolumes) {
			t.Errorf("%s: Volume count mismatch! Expected %d, got %d", test.name, len(test.expectedVolumes), len(gotVolumes))
			continue
		}
		for k, exp := range test.expectedVolumes {
			g, ok := gotVolumes[k]
			if !ok {
				t.Errorf("%s: Didn't find expected volume %s", test.name, k)
			}

			if g != exp {
				t.Errorf("%s: Expected volume of type %s, got %s", test.name, g, exp)
			}
		}

		if test.expectedName != res.Name {
			t.Errorf("%s: Unexpected name: %s", test.name, test.expectedName)
		}

		if test.expectInsecure == nil {
			continue
		}
		for _, stream := range imageStreams {
			_, hasAnnotation := stream.Annotations[imageapi.InsecureRepositoryAnnotation]
			if test.expectInsecure.Has(stream.Name) && !hasAnnotation {
				t.Errorf("%s: Expected insecure annotation for stream: %s, but did not get one.", test.name, stream.Name)
			}
			if !test.expectInsecure.Has(stream.Name) && hasAnnotation {
				t.Errorf("%s: Got insecure annotation for stream: %s, and was not expecting one.", test.name, stream.Name)
			}
		}

	}
}

func TestNewAppRunBuilds(t *testing.T) {
	skipExternalGit(t)
	tests := []struct {
		name   string
		config *cmd.AppConfig

		expected    map[string][]string
		expectedErr func(error) bool
		checkResult func(*cmd.AppResult) error
		checkOutput func(stdout, stderr io.Reader) error
	}{

		{
			name: "successful ruby app generation",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},
				DockerImages:       []string{"centos/ruby-22-centos7", "centos/mongodb-26-centos7"},
				OutputDocker:       true,
			},
			expected: map[string][]string{
				// TODO: this test used to silently ignore components that were not builders (i.e. user input)
				//   That's bad, so the code should either error in this case or be a bit smarter.
				"buildConfig": {"ruby-hello-world", "ruby-hello-world-1"},
				"imageStream": {"mongodb-26-centos7", "ruby-22-centos7"},
			},
		},
		{
			name: "successful build from dockerfile",
			config: &cmd.AppConfig{
				Dockerfile: "FROM openshift/origin:v1.0.6\nUSER foo",
			},
			expected: map[string][]string{
				"buildConfig": {"origin"},
				// There's a single image stream, but different tags: input from
				// openshift/origin:v1.0.6, output to openshift/origin:latest.
				"imageStream": {"origin"},
			},
		},
		{
			name: "successful build with no output",
			config: &cmd.AppConfig{
				Dockerfile: "FROM centos",
				NoOutput:   true,
			},
			expected: map[string][]string{
				"buildConfig": {"centos"},
				"imageStream": {"centos"},
			},
			checkResult: func(res *cmd.AppResult) error {
				for _, item := range res.List.Items {
					switch t := item.(type) {
					case *buildapi.BuildConfig:
						got := t.Spec.Output.To
						want := (*kapi.ObjectReference)(nil)
						if !reflect.DeepEqual(got, want) {
							return fmt.Errorf("build.Spec.Output.To = %v; want %v", got, want)
						}
						return nil
					}
				}
				return fmt.Errorf("BuildConfig not found; got %v", res.List.Items)
			},
		},
		{
			name: "successful build from dockerfile with custom name",
			config: &cmd.AppConfig{
				Dockerfile: "FROM openshift/origin-base\nUSER foo",
				Name:       "foobar",
			},
			expected: map[string][]string{
				"buildConfig": {"foobar"},
				"imageStream": {"origin-base", "foobar"},
			},
		},
		{
			name: "successful build from dockerfile with --to",
			config: &cmd.AppConfig{
				Dockerfile: "FROM openshift/origin-base\nUSER foo",
				Name:       "foobar",
				To:         "destination/reference:tag",
			},
			expected: map[string][]string{
				"buildConfig": {"foobar"},
				"imageStream": {"origin-base", "reference"},
			},
		},
		{
			name: "successful build from dockerfile with --to and --to-docker=true",
			config: &cmd.AppConfig{
				Dockerfile:   "FROM openshift/origin-base\nUSER foo",
				Name:         "foobar",
				To:           "destination/reference:tag",
				OutputDocker: true,
			},
			expected: map[string][]string{
				"buildConfig": {"foobar"},
				"imageStream": {"origin-base"},
			},
			checkResult: func(res *cmd.AppResult) error {
				for _, item := range res.List.Items {
					switch t := item.(type) {
					case *buildapi.BuildConfig:
						got := t.Spec.Output.To
						want := &kapi.ObjectReference{
							Kind: "DockerImage",
							Name: "destination/reference:tag",
						}
						if !reflect.DeepEqual(got, want) {
							return fmt.Errorf("build.Spec.Output.To = %v; want %v", got, want)
						}
						return nil
					}
				}
				return fmt.Errorf("BuildConfig not found; got %v", res.List.Items)
			},
		},
		{
			name: "successful build from dockerfile with identical input and output image references with warning",
			config: &cmd.AppConfig{
				Dockerfile: "FROM centos\nRUN yum install -y httpd",
				To:         "centos",
			},
			expected: map[string][]string{
				"buildConfig": {"centos"},
				"imageStream": {"centos"},
			},
			checkOutput: func(stdout, stderr io.Reader) error {
				got, err := ioutil.ReadAll(stderr)
				if err != nil {
					return err
				}
				want := "--> WARNING: the input and output image stream tags are identical (\"docker.io/library/centos:latest\")\n"
				if string(got) != want {
					return fmt.Errorf("stderr: got %q; want %q", got, want)
				}
				return nil
			},
		},
		{
			name: "successful generation of BC with multiple sources: repo + Dockerfile",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},
				Dockerfile:         "FROM centos/ruby-22-centos7\nRUN false",
			},
			expected: map[string][]string{
				"buildConfig": {"ruby-hello-world"},
				"imageStream": {"ruby-22-centos7", "ruby-hello-world"},
			},
			checkResult: func(res *cmd.AppResult) error {
				var bc *buildapi.BuildConfig
				for _, item := range res.List.Items {
					switch v := item.(type) {
					case *buildapi.BuildConfig:
						if bc != nil {
							return fmt.Errorf("want one BuildConfig got multiple: %#v", res.List.Items)
						}
						bc = v
					}
				}
				if bc == nil {
					return fmt.Errorf("want one BuildConfig got none: %#v", res.List.Items)
				}
				var got string
				if bc.Spec.Source.Dockerfile != nil {
					got = *bc.Spec.Source.Dockerfile
				}
				want := "FROM centos/ruby-22-centos7\nRUN false"
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Dockerfile = %q; want %q", got, want)
				}
				return nil
			},
		},
		{
			name: "unsuccessful build from dockerfile due to strategy conflict",
			config: &cmd.AppConfig{
				Dockerfile: "FROM openshift/origin-base\nUSER foo",
				Strategy:   "source",
			},
			expectedErr: func(err error) bool {
				return err.Error() == "when directly referencing a Dockerfile, the strategy must must be 'docker'"
			},
		},
		{
			name: "unsuccessful build from dockerfile due to missing FROM instruction",
			config: &cmd.AppConfig{
				Dockerfile: "USER foo",
				Strategy:   "docker",
			},
			expectedErr: func(err error) bool {
				return err.Error() == "the Dockerfile in the repository \"\" has no FROM instruction"
			},
		},
		{
			name: "unsuccessful build from dockerfile due to identical input and output image references",
			config: &cmd.AppConfig{
				Dockerfile: "FROM centos\nRUN yum install -y httpd",
			},
			expectedErr: func(err error) bool {
				e := app.CircularOutputReferenceError{
					Reference: imageapi.DockerImageReference{
						Name: "centos",
					}.DockerClientDefaults(),
				}
				return err.Error() == fmt.Errorf("%v, please specify a different output reference with --to", e).Error()
			},
		},
		{
			name: "unsuccessful generation of BC with multiple repos and Dockerfile",
			config: &cmd.AppConfig{
				SourceRepositories: []string{
					"https://github.com/openshift/ruby-hello-world",
					"https://github.com/openshift/django-ex",
				},
				Dockerfile: "FROM centos/ruby-22-centos7\nRUN false",
			},
			expectedErr: func(err error) bool {
				return err.Error() == "--dockerfile cannot be used with multiple source repositories"
			},
		},

		{
			name: "successful input image source build with a repository",
			config: &cmd.AppConfig{
				SourceRepositories: []string{
					"https://github.com/openshift/ruby-hello-world",
				},
				SourceImage:     "centos/mongodb-26-centos7",
				SourceImagePath: "/src:dst",
			},
			expected: map[string][]string{
				"buildConfig": {"ruby-hello-world"},
				"imageStream": {"mongodb-26-centos7", "ruby-22-centos7", "ruby-hello-world"},
			},
			checkResult: func(res *cmd.AppResult) error {
				var bc *buildapi.BuildConfig
				for _, item := range res.List.Items {
					switch v := item.(type) {
					case *buildapi.BuildConfig:
						if bc != nil {
							return fmt.Errorf("want one BuildConfig got multiple: %#v", res.List.Items)
						}
						bc = v
					}
				}
				if bc == nil {
					return fmt.Errorf("want one BuildConfig got none: %#v", res.List.Items)
				}
				var got string

				want := "mongodb-26-centos7:latest"
				got = bc.Spec.Source.Images[0].From.Name
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.From.Name = %q; want %q", got, want)
				}

				want = "ImageStreamTag"
				got = bc.Spec.Source.Images[0].From.Kind
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.From.Kind = %q; want %q", got, want)
				}

				want = "/src"
				got = bc.Spec.Source.Images[0].Paths[0].SourcePath
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.Paths[0].SourcePath = %q; want %q", got, want)
				}

				want = "dst"
				got = bc.Spec.Source.Images[0].Paths[0].DestinationDir
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.Paths[0].DestinationDir = %q; want %q", got, want)
				}
				return nil
			},
		},
		{
			name: "successful input image source build with no repository",
			config: &cmd.AppConfig{
				Components:      []string{"centos/mysql-56-centos7"},
				To:              "outputimage",
				SourceImage:     "centos/mongodb-26-centos7",
				SourceImagePath: "/src:dst",
			},
			expected: map[string][]string{
				"buildConfig": {"outputimage"},
				"imageStream": {"mongodb-26-centos7", "mysql-56-centos7", "outputimage"},
			},
			checkResult: func(res *cmd.AppResult) error {
				var bc *buildapi.BuildConfig
				for _, item := range res.List.Items {
					switch v := item.(type) {
					case *buildapi.BuildConfig:
						if bc != nil {
							return fmt.Errorf("want one BuildConfig got multiple: %#v", res.List.Items)
						}
						bc = v
					}
				}
				if bc == nil {
					return fmt.Errorf("want one BuildConfig got none: %#v", res.List.Items)
				}
				var got string

				want := "mongodb-26-centos7:latest"
				got = bc.Spec.Source.Images[0].From.Name
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.From.Name = %q; want %q", got, want)
				}

				want = "ImageStreamTag"
				got = bc.Spec.Source.Images[0].From.Kind
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.From.Kind = %q; want %q", got, want)
				}

				want = "/src"
				got = bc.Spec.Source.Images[0].Paths[0].SourcePath
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.Paths[0].SourcePath = %q; want %q", got, want)
				}

				want = "dst"
				got = bc.Spec.Source.Images[0].Paths[0].DestinationDir
				if got != want {
					return fmt.Errorf("bc.Spec.Source.Image.Paths[0].DestinationDir = %q; want %q", got, want)
				}
				return nil
			},
		},
	}
	for _, test := range tests {
		stdout, stderr := PrepareAppConfig(test.config)

		res, err := test.config.Run()
		if (test.expectedErr == nil && err != nil) || (test.expectedErr != nil && !test.expectedErr(err)) {
			t.Errorf("%s: unexpected error: %v", test.name, err)
			continue
		}
		if err != nil {
			continue
		}
		if test.checkOutput != nil {
			if err := test.checkOutput(stdout, stderr); err != nil {
				t.Error(err)
				continue
			}
		}
		got := map[string][]string{}
		for _, obj := range res.List.Items {
			switch tp := obj.(type) {
			case *buildapi.BuildConfig:
				got["buildConfig"] = append(got["buildConfig"], tp.Name)
			case *imageapi.ImageStream:
				got["imageStream"] = append(got["imageStream"], tp.Name)
			}
		}

		if len(test.expected) != len(got) {
			t.Errorf("%s: Resource kind size mismatch! Expected %d, got %d", test.name, len(test.expected), len(got))
			continue
		}

		for k, exp := range test.expected {
			g, ok := got[k]
			if !ok {
				t.Errorf("%s: Didn't find expected kind %s", test.name, k)
			}

			sort.Strings(g)
			sort.Strings(exp)

			if !reflect.DeepEqual(g, exp) {
				t.Errorf("%s: Resource names mismatch! Expected %v, got %v", test.name, exp, g)
				continue
			}
		}

		if test.checkResult != nil {
			if err := test.checkResult(res); err != nil {
				t.Errorf("%s: unexpected result: %v", test.name, err)
			}
		}
	}
}

// PrepareNewAppAppConfig sets fields in config appropriate for running tests. It
// returns two buffers bound to stdout and stderr.
func PrepareNewAppAppConfig(config *cmd.AppConfig) (stdout, stderr *bytes.Buffer) {
	config.ExpectToBuild = true
	stdout, stderr = new(bytes.Buffer), new(bytes.Buffer)
	config.Out, config.ErrOut = stdout, stderr

	config.Detector = app.SourceRepositoryEnumerator{
		Detectors: source.DefaultDetectors,
		Tester:    dockerfile.NewTester(),
	}
	config.DockerSearcher = app.DockerRegistrySearcher{
		Client: dockerregistry.NewClient(10*time.Second, true),
	}
	config.ImageStreamByAnnotationSearcher = fakeImageStreamSearcher()
	config.ImageStreamSearcher = fakeImageStreamSearcher()
	config.OriginNamespace = "default"
	config.OSClient = &client.Fake{}
	config.RefBuilder = &app.ReferenceBuilder{}
	config.TemplateSearcher = app.TemplateSearcher{
		Client: &client.Fake{},
		TemplateConfigsNamespacer: &client.Fake{},
		Namespaces:                []string{"openshift", "default"},
	}
	config.Typer = kapi.Scheme
	return
}

func TestNewAppNewBuildEnvVars(t *testing.T) {
	skipExternalGit(t)
	dockerSearcher := app.DockerRegistrySearcher{
		Client: dockerregistry.NewClient(10*time.Second, true),
	}

	tests := []struct {
		name        string
		config      *cmd.AppConfig
		expected    []kapi.EnvVar
		expectedErr error
	}{
		{
			name: "explicit environment variables for buildConfig and deploymentConfig",
			config: &cmd.AppConfig{
				AddEnvironmentToBuild: true,
				SourceRepositories:    []string{"https://github.com/openshift/ruby-hello-world"},
				DockerImages:          []string{"centos/ruby-22-centos7", "centos/mongodb-26-centos7"},
				OutputDocker:          true,
				Environment:           []string{"BUILD_ENV_1=env_value_1", "BUILD_ENV_2=env_value_2"},
				DockerSearcher:        dockerSearcher,
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},
			expected: []kapi.EnvVar{
				{Name: "BUILD_ENV_1", Value: "env_value_1"},
				{Name: "BUILD_ENV_2", Value: "env_value_2"},
			},
			expectedErr: nil,
		},
	}

	for _, test := range tests {
		test.config.RefBuilder = &app.ReferenceBuilder{}
		test.config.Out, test.config.ErrOut = os.Stdout, os.Stderr
		test.config.ExpectToBuild = true
		res, err := test.config.Run()
		if err != test.expectedErr {
			t.Errorf("%s: Error mismatch! Expected %v, got %v", test.name, test.expectedErr, err)
			continue
		}
		got := []kapi.EnvVar{}
		for _, obj := range res.List.Items {
			switch tp := obj.(type) {
			case *buildapi.BuildConfig:
				got = tp.Spec.Strategy.SourceStrategy.Env
				break
			}
		}

		if !reflect.DeepEqual(test.expected, got) {
			t.Errorf("%s: unexpected output. Expected: %#v, Got: %#v", test.name, test.expected, got)
			continue
		}
	}
}

func TestNewAppBuildConfigEnvVarsAndSecrets(t *testing.T) {
	skipExternalGit(t)
	dockerSearcher := app.DockerRegistrySearcher{
		Client: dockerregistry.NewClient(10*time.Second, true),
	}

	tests := []struct {
		name            string
		config          *cmd.AppConfig
		expected        []kapi.EnvVar
		expectedSecrets map[string]string
		expectedErr     error
	}{
		{
			name: "explicit environment variables for buildConfig and deploymentConfig",
			config: &cmd.AppConfig{
				SourceRepositories: []string{"https://github.com/openshift/ruby-hello-world"},
				DockerImages:       []string{"centos/ruby-22-centos7", "centos/mongodb-26-centos7"},
				OutputDocker:       true,
				Environment:        []string{"BUILD_ENV_1=env_value_1", "BUILD_ENV_2=env_value_2"},
				Secrets:            []string{"foo:/var", "bar"},
				DockerSearcher:     dockerSearcher,
				Detector: app.SourceRepositoryEnumerator{
					Detectors: source.DefaultDetectors,
					Tester:    dockerfile.NewTester(),
				},
				Typer:           kapi.Scheme,
				OSClient:        &client.Fake{},
				OriginNamespace: "default",
			},
			expected:        []kapi.EnvVar{},
			expectedSecrets: map[string]string{"foo": "/var", "bar": "."},
			expectedErr:     nil,
		},
	}

	for _, test := range tests {
		test.config.RefBuilder = &app.ReferenceBuilder{}
		test.config.Out, test.config.ErrOut = os.Stdout, os.Stderr
		test.config.Deploy = true
		res, err := test.config.Run()
		if err != test.expectedErr {
			t.Errorf("%s: Error mismatch! Expected %v, got %v", test.name, test.expectedErr, err)
			continue
		}
		got := []kapi.EnvVar{}
		gotSecrets := []buildapi.SecretBuildSource{}
		for _, obj := range res.List.Items {
			switch tp := obj.(type) {
			case *buildapi.BuildConfig:
				got = tp.Spec.Strategy.SourceStrategy.Env
				gotSecrets = tp.Spec.Source.Secrets
				break
			}
		}

		for secretName, destDir := range test.expectedSecrets {
			found := false
			for _, got := range gotSecrets {
				if got.Secret.Name == secretName && got.DestinationDir == destDir {
					found = true
					continue
				}
			}
			if !found {
				t.Errorf("expected secret %q and destination %q, got %#v", secretName, destDir, gotSecrets)
				continue
			}
		}

		if !reflect.DeepEqual(test.expected, got) {
			t.Errorf("%s: unexpected output. Expected: %#v, Got: %#v", test.name, test.expected, got)
			continue
		}
	}
}

func builderImageStream() *imageapi.ImageStream {
	return &imageapi.ImageStream{
		ObjectMeta: kapi.ObjectMeta{
			Name:            "ruby",
			ResourceVersion: "1",
		},
		Status: imageapi.ImageStreamStatus{
			Tags: map[string]imageapi.TagEventList{
				"latest": {
					Items: []imageapi.TagEvent{
						{
							Image: "the-image-id",
						},
					},
				},
			},
			DockerImageRepository: "example/ruby:latest",
		},
	}

}

func builderImageStreams() *imageapi.ImageStreamList {
	return &imageapi.ImageStreamList{
		Items: []imageapi.ImageStream{*builderImageStream()},
	}
}

func builderImage() *imageapi.ImageStreamImage {
	return &imageapi.ImageStreamImage{
		Image: imageapi.Image{
			DockerImageReference: "example/ruby:latest",
			DockerImageMetadata: imageapi.DockerImage{
				Config: &imageapi.DockerConfig{
					Env: []string{
						"STI_SCRIPTS_URL=http://repo/git/ruby",
					},
					ExposedPorts: map[string]struct{}{
						"8080/tcp": {},
					},
				},
			},
		},
	}
}

func dockerBuilderImage() *docker.Image {
	return &docker.Image{
		ID: "ruby",
		Config: &docker.Config{
			Env: []string{
				"STI_SCRIPTS_URL=http://repo/git/ruby",
			},
			ExposedPorts: map[docker.Port]struct{}{
				"8080/tcp": {},
			},
		},
	}
}

func fakeImageStreamSearcher() app.Searcher {
	client := &client.Fake{}
	client.AddReactor("get", "imagestreams", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
		return true, builderImageStream(), nil
	})
	client.AddReactor("list", "imagestreams", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
		return true, builderImageStreams(), nil
	})
	client.AddReactor("get", "imagestreamimages", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
		return true, builderImage(), nil
	})

	return app.ImageStreamSearcher{
		Client:            client,
		ImageStreamImages: client,
		Namespaces:        []string{"default"},
	}
}

func fakeTemplateSearcher() app.Searcher {
	client := &client.Fake{}
	client.AddReactor("list", "templates", func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
		return true, templateList(), nil
	})

	return app.TemplateSearcher{
		Client:     client,
		Namespaces: []string{"default"},
	}
}

func templateList() *templateapi.TemplateList {
	return &templateapi.TemplateList{
		Items: []templateapi.Template{
			{
				Objects: []runtime.Object{},
				ObjectMeta: kapi.ObjectMeta{
					Name:      "first-stored-template",
					Namespace: "default",
				},
			},
		},
	}
}

func fakeDockerSearcher() app.Searcher {
	return app.DockerClientSearcher{
		Client: &dockertools.FakeDockerClient{
			Images: []docker.APIImages{{RepoTags: []string{"library/ruby:latest"}}},
			Image:  dockerBuilderImage(),
		},
		Insecure:         true,
		RegistrySearcher: &ExactMatchDockerSearcher{},
	}
}

func fakeSimpleDockerSearcher() app.Searcher {
	return app.DockerClientSearcher{
		Client: &dockertools.FakeDockerClient{
			Images: []docker.APIImages{{RepoTags: []string{"centos/ruby-22-centos7"}}},
			Image: &docker.Image{
				ID: "ruby",
				Config: &docker.Config{
					Env: []string{},
				},
			},
		},
		RegistrySearcher: &ExactMatchDockerSearcher{},
	}
}

func createLocalGitDirectory(t *testing.T) string {
	dir, err := ioutil.TempDir(os.TempDir(), "s2i-test")
	if err != nil {
		t.Error(err)
	}
	os.Mkdir(filepath.Join(dir, ".git"), 0600)
	return dir
}

// MockSourceRepositories is a set of mocked source repositories used for
// testing
func MockSourceRepositories(t *testing.T, file string) []*app.SourceRepository {
	var b []*app.SourceRepository
	for _, location := range []string{
		"https://github.com/openshift/ruby-hello-world.git",
		file,
	} {
		s, err := app.NewSourceRepository(location)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, s)
	}
	return b
}

// PrepareAppConfig sets fields in config appropriate for running tests. It
// returns two buffers bound to stdout and stderr.
func PrepareAppConfig(config *cmd.AppConfig) (stdout, stderr *bytes.Buffer) {
	config.ExpectToBuild = true
	stdout, stderr = new(bytes.Buffer), new(bytes.Buffer)
	config.Out, config.ErrOut = stdout, stderr

	config.Detector = app.SourceRepositoryEnumerator{
		Detectors: source.DefaultDetectors,
		Tester:    dockerfile.NewTester(),
	}
	config.DockerSearcher = app.DockerRegistrySearcher{
		Client: dockerregistry.NewClient(10*time.Second, true),
	}
	config.ImageStreamByAnnotationSearcher = fakeImageStreamSearcher()
	config.ImageStreamSearcher = fakeImageStreamSearcher()
	config.OriginNamespace = "default"
	config.OSClient = &client.Fake{}
	config.RefBuilder = &app.ReferenceBuilder{}
	config.TemplateSearcher = app.TemplateSearcher{
		Client: &client.Fake{},
		TemplateConfigsNamespacer: &client.Fake{},
		Namespaces:                []string{"openshift", "default"},
	}
	config.Typer = kapi.Scheme
	return
}
