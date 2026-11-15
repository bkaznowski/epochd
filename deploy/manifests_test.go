package deploy_test

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// These tests read the manifests as Kubernetes would: strictly decoded into the
// real API types, so a misspelt field is an error rather than silently ignored.
// They then check the things that have to agree across files, which no single
// `kubectl apply --dry-run` would catch.

// docs splits a multi-document YAML file.
func docs(t *testing.T, file string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, d := range bytes.Split(b, []byte("\n---")) {
		if len(bytes.TrimSpace(d)) > 0 {
			out = append(out, d)
		}
	}
	return out
}

// strict decodes one document into v, rejecting unknown fields.
func strict(t *testing.T, doc []byte, v any) {
	t.Helper()
	if err := yaml.UnmarshalStrict(doc, v); err != nil {
		t.Fatalf("decoding:\n%s\n%v", doc, err)
	}
}

func daemonSet(t *testing.T) appsv1.DaemonSet {
	t.Helper()
	var ds appsv1.DaemonSet
	strict(t, docs(t, "daemonset.yaml")[0], &ds)
	return ds
}

func controllerDeployment(t *testing.T) appsv1.Deployment {
	t.Helper()
	for _, d := range docs(t, "controller-deployment.yaml") {
		if strings.Contains(string(d), "kind: Deployment") {
			var dep appsv1.Deployment
			strict(t, d, &dep)
			return dep
		}
	}
	t.Fatal("no Deployment in controller-deployment.yaml")
	return appsv1.Deployment{}
}

// flagValue returns the value of --name=value among args.
func flagValue(args []string, name string) (string, bool) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestEveryManifestDecodesStrictly(t *testing.T) {
	for _, f := range []string{"rbac.yaml", "daemonset.yaml", "controller-deployment.yaml"} {
		for _, d := range docs(t, f) {
			var meta struct {
				Kind string `json:"kind"`
			}
			if err := yaml.Unmarshal(d, &meta); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			var target any
			switch meta.Kind {
			case "DaemonSet":
				target = &appsv1.DaemonSet{}
			case "Deployment":
				target = &appsv1.Deployment{}
			case "Service":
				target = &corev1.Service{}
			case "ServiceAccount":
				target = &corev1.ServiceAccount{}
			case "Namespace":
				target = &corev1.Namespace{}
			case "ClusterRole":
				target = &rbacv1.ClusterRole{}
			case "ClusterRoleBinding":
				target = &rbacv1.ClusterRoleBinding{}
			case "Role":
				target = &rbacv1.Role{}
			case "RoleBinding":
				target = &rbacv1.RoleBinding{}
			default:
				t.Errorf("%s: no decoder for kind %q; add one so it is checked", f, meta.Kind)
				continue
			}
			strict(t, d, target)
		}
	}
}

// The controller dials the agents on --agent-port; the agent listens on
// --listen and publishes the same port on the host. Any drift between the three
// leaves the controller talking to nothing.
func TestAgentPortAgreesAcrossTheManifests(t *testing.T) {
	ds := daemonSet(t)
	agent := ds.Spec.Template.Spec.Containers[0]

	listen, ok := flagValue(agent.Args, "listen")
	if !ok {
		t.Fatal("the agent has no --listen argument")
	}
	port := strings.TrimPrefix(listen, ":")
	if port == "9100" {
		t.Error("the agent uses 9100, node-exporter's default port, which it collides with under hostNetwork")
	}

	if len(agent.Ports) != 1 {
		t.Fatalf("the agent declares %d ports, want 1", len(agent.Ports))
	}
	p := agent.Ports[0]
	if got := itoa(p.ContainerPort); got != port || itoa(p.HostPort) != port {
		t.Errorf("--listen=%s but containerPort=%d hostPort=%d", listen, p.ContainerPort, p.HostPort)
	}

	ctrl := controllerDeployment(t).Spec.Template.Spec.Containers[0]
	agentPort, ok := flagValue(ctrl.Args, "agent-port")
	if !ok || agentPort != port {
		t.Errorf("the controller dials --agent-port=%q but the agent listens on %s", agentPort, port)
	}

	for _, probe := range []*corev1.Probe{agent.ReadinessProbe, agent.LivenessProbe} {
		if probe == nil || probe.TCPSocket == nil || itoa(probe.TCPSocket.Port.IntVal) != port {
			t.Errorf("the agent's probes must check port %s: %+v", port, probe)
		}
	}
}

func itoa(n int32) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(int(n))
}

// Both binaries refuse to start without credentials, so the manifests must
// supply them, from a Secret whose keys they actually mount.
func TestCredentialsAreWiredFromTheSecret(t *testing.T) {
	agentSpec := daemonSet(t).Spec.Template.Spec
	ctrlSpec := controllerDeployment(t).Spec.Template.Spec

	agentToken, ok := flagValue(agentSpec.Containers[0].Args, "auth-token-file")
	if !ok {
		t.Fatal("the agent is started without --auth-token-file and would refuse to run")
	}
	apiToken, ok1 := flagValue(ctrlSpec.Containers[0].Args, "auth-token-file")
	agentSide, ok2 := flagValue(ctrlSpec.Containers[0].Args, "agent-token-file")
	if !ok1 || !ok2 {
		t.Fatalf("the controller needs --auth-token-file and --agent-token-file (got %v)", ctrlSpec.Containers[0].Args)
	}

	// Each file the binaries read must be a file the Secret volume provides.
	provides := func(spec corev1.PodSpec, mount string) map[string]bool {
		out := map[string]bool{}
		for _, v := range spec.Volumes {
			if v.Secret == nil || v.Secret.SecretName != "epochd-auth" {
				continue
			}
			var m *corev1.VolumeMount
			for i := range spec.Containers[0].VolumeMounts {
				if spec.Containers[0].VolumeMounts[i].Name == v.Name {
					m = &spec.Containers[0].VolumeMounts[i]
				}
			}
			if m == nil {
				t.Fatalf("volume %q is never mounted", v.Name)
			}
			if len(v.Secret.Items) == 0 { // whole Secret: every key becomes a file
				out[m.MountPath+"/api-token"] = true
				out[m.MountPath+"/agent-token"] = true
			}
			for _, it := range v.Secret.Items {
				out[m.MountPath+"/"+it.Path] = true
				if it.Key != "agent-token" && it.Key != "api-token" {
					t.Errorf("volume projects key %q, which the Makefile's secret target never creates", it.Key)
				}
			}
		}
		return out
	}
	if got := provides(agentSpec, ""); !got[agentToken] {
		t.Errorf("the agent reads %s but its Secret volume provides %v", agentToken, got)
	}
	ctrlFiles := provides(ctrlSpec, "")
	for _, f := range []string{apiToken, agentSide} {
		if !ctrlFiles[f] {
			t.Errorf("the controller reads %s but its Secret volume provides %v", f, ctrlFiles)
		}
	}

	// The agent must not be handed the token API clients use.
	for f := range provides(agentSpec, "") {
		if strings.HasSuffix(f, "api-token") {
			t.Errorf("the agent can read the controller's API token (%s)", f)
		}
	}
}

// Events the controller posts need the permission to create them, or the API
// server drops them without a word.
func TestControllerMayPostEvents(t *testing.T) {
	for _, d := range docs(t, "rbac.yaml") {
		if !strings.Contains(string(d), "kind: ClusterRole\n") {
			continue
		}
		var cr rbacv1.ClusterRole
		strict(t, d, &cr)
		for _, r := range cr.Rules {
			for _, res := range r.Resources {
				if res == "events" && contains(r.Verbs, "create") && contains(r.Verbs, "patch") {
					return
				}
			}
		}
		t.Errorf("ClusterRole %s cannot create and patch events: %+v", cr.Name, cr.Rules)
		return
	}
	t.Fatal("no ClusterRole found")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// A rolling update would briefly run two controllers that both sweep, persist to
// the same ConfigMap and inject into the same pods.
func TestControllerIsNeverRunTwice(t *testing.T) {
	dep := controllerDeployment(t)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", dep.Spec.Replicas)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("strategy = %q, want Recreate", dep.Spec.Strategy.Type)
	}
}

// The agent can ptrace any process on its node, so it gets the one capability that
// takes and nothing else. This was privileged: true until the end-to-end suite
// showed CAP_SYS_PTRACE is sufficient; a regression to a broader context should be
// a conscious change, not an edit that slips through.
func TestAgentIsNotPrivileged(t *testing.T) {
	sc := daemonSet(t).Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("the agent has no securityContext")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("the agent is privileged; CAP_SYS_PTRACE is enough")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("the root filesystem should be read-only")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccompProfile = %+v, want RuntimeDefault", sc.SeccompProfile)
	}
	if sc.Capabilities == nil {
		t.Fatal("no capabilities block")
	}
	var drop, add []string
	for _, c := range sc.Capabilities.Drop {
		drop = append(drop, string(c))
	}
	for _, c := range sc.Capabilities.Add {
		add = append(add, string(c))
	}
	if !contains(drop, "ALL") {
		t.Errorf("capabilities.drop = %v, want ALL", drop)
	}
	if len(add) != 1 || add[0] != "SYS_PTRACE" {
		t.Errorf("capabilities.add = %v, want exactly [SYS_PTRACE]", add)
	}
}
