# Third-Party Notices and Attribution

This project depends on open-source software. The Go module list below is generated from `go.sum` and includes direct, indirect, transitive, and test/tooling modules resolved during builds.

Generated from:

```bash
awk '{print $1}' go.sum | sed 's@/go.mod$@@' | sort -u
```

## Go Modules
- `github.com/creack/pty`
- `github.com/davecgh/go-spew`
- `github.com/emicklei/go-restful/v3`
- `github.com/go-logr/logr`
- `github.com/go-openapi/jsonpointer`
- `github.com/go-openapi/jsonreference`
- `github.com/go-openapi/swag`
- `github.com/go-task/slim-sprig`
- `github.com/gogo/protobuf`
- `github.com/golang/protobuf`
- `github.com/google/gnostic-models`
- `github.com/google/go-cmp`
- `github.com/google/gofuzz`
- `github.com/google/pprof`
- `github.com/google/uuid`
- `github.com/imdario/mergo`
- `github.com/josharian/intern`
- `github.com/json-iterator/go`
- `github.com/kisielk/errcheck`
- `github.com/kisielk/gotool`
- `github.com/kr/pretty`
- `github.com/kr/pty`
- `github.com/kr/text`
- `github.com/mailru/easyjson`
- `github.com/modern-go/concurrent`
- `github.com/modern-go/reflect2`
- `github.com/munnerz/goautoneg`
- `github.com/onsi/ginkgo/v2`
- `github.com/onsi/gomega`
- `github.com/pmezard/go-difflib`
- `github.com/rogpeppe/go-internal`
- `github.com/spf13/pflag`
- `github.com/stretchr/objx`
- `github.com/stretchr/testify`
- `github.com/yuin/goldmark`
- `golang.org/x/crypto`
- `golang.org/x/mod`
- `golang.org/x/net`
- `golang.org/x/oauth2`
- `golang.org/x/sync`
- `golang.org/x/sys`
- `golang.org/x/term`
- `golang.org/x/text`
- `golang.org/x/time`
- `golang.org/x/tools`
- `golang.org/x/xerrors`
- `google.golang.org/appengine`
- `google.golang.org/protobuf`
- `gopkg.in/check.v1`
- `gopkg.in/inf.v0`
- `gopkg.in/yaml.v2`
- `gopkg.in/yaml.v3`
- `k8s.io/api`
- `k8s.io/apimachinery`
- `k8s.io/client-go`
- `k8s.io/klog/v2`
- `k8s.io/kube-openapi`
- `k8s.io/utils`
- `sigs.k8s.io/json`
- `sigs.k8s.io/structured-merge-diff/v4`
- `sigs.k8s.io/yaml`

## Runtime and Platform Components

The deployment and runtime also rely on these upstream open-source projects:

- Kubernetes
- Gateway API
- Istio
- cert-manager
- Go
- Docker / OCI image tooling
- Distroless container images

Please consult each dependency's repository for license terms and notices.
