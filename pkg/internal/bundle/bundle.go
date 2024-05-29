package bundle

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"text/template"
	"time"

	"chainguard.dev/apko/pkg/build/types"
	"chainguard.dev/melange/pkg/config"
	"github.com/dominikbraun/graph"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	ggcrtypes "github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/ptr"
)

const MaxArmCore = 48

type GCSFuseMount struct {
	Bucket  string
	Mount   string
	OnlyDir string
}

func ParseGCSFuseMount(s string) (*GCSFuseMount, error) {
	before, mount, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("gcsfuse mount spec should be in the form 'bucket[/onlydir]:/mount', got: %q", s)
	}

	bucket, onlydir, _ := strings.Cut(before, "/")

	return &GCSFuseMount{
		Bucket:  bucket,
		Mount:   mount,
		OnlyDir: onlydir,
	}, nil
}

type Entrypoint struct {
	Flags         []string
	GCSFuseMounts []*GCSFuseMount
}

const entrypointTemplate = `# generated by wolfictl bundle
set -eux

{{ range .GCSFuseMounts }}
mkdir -p {{ .Mount }}
gcsfuse -o ro --implicit-dirs {{ if .OnlyDir }} --only-dir {{ .OnlyDir }} {{ end }} {{ .Bucket }} {{ .Mount }}
{{ end }}

# TODO: Should this be in the bundle?
melange keygen local-melange.rsa

# Generate this source-dir in case it doesn't exist.
# TODO: We shouldn't need this.
mkdir -p $2

melange build $1 \
 --gcplog \
 --source-dir $2 \
{{ range .Flags }} {{.}} \
{{ end }}

tar -C packages -czvf packages.tar.gz .

# TODO: Content-Type
curl --upload-file packages.tar.gz -H "Content-Type: application/octet-stream" $PACKAGES_UPLOAD_URL

sha256sum packages.tar.gz
sha256sum packages.tar.gz | cut -d' ' -f1 > /dev/termination-log
`

var entrypointTmpl *template.Template

func init() {
	entrypointTmpl = template.Must(template.New("entrypointTemplate").Parse(entrypointTemplate))
}

func renderEntrypoint(entrypoint *Entrypoint) (v1.Layer, error) {
	var tbuf bytes.Buffer
	tw := tar.NewWriter(&tbuf)

	var ebuf bytes.Buffer
	if err := entrypointTmpl.Execute(&ebuf, entrypoint); err != nil {
		return nil, err
	}

	eb := ebuf.Bytes()

	hdr := &tar.Header{
		Name: "entrypoint.sh",
		Mode: 0o755,
		Size: int64(len(eb)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}

	if _, err := tw.Write(eb); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	opener := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(tbuf.Bytes())), nil
	}

	return tarball.LayerFromOpener(opener)
}

// todo: optimize this if it matters (it probably doesn't)
func layer(srcfs fs.FS) (v1.Layer, error) {
	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)
	if err := tarAddFS(tw, srcfs); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	opener := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	}

	return tarball.LayerFromOpener(opener)
}

func New(base v1.ImageIndex, entrypoints map[types.Architecture]*Entrypoint, commonfiles, srcfs fs.FS) (v1.ImageIndex, error) {
	m, err := base.IndexManifest()
	if err != nil {
		return nil, err
	}

	wantArchs := map[types.Architecture]struct{}{}
	for arch := range entrypoints {
		wantArchs[arch] = struct{}{}
	}

	haveArchs := map[types.Architecture]v1.Descriptor{}
	for _, desc := range m.Manifests { //nolint: gocritic
		haveArchs[types.ParseArchitecture(desc.Platform.Architecture)] = desc
	}

	var idx v1.ImageIndex = empty.Index

	for arch := range wantArchs {
		platform := &v1.Platform{
			OS:           "linux", // TODO: If this is ever wrong, throw a party.
			Architecture: string(arch),
		}

		baseImg := mutate.MediaType(empty.Image, ggcrtypes.OCIManifestSchema1)
		if desc, ok := haveArchs[arch]; ok {
			baseImg, err = base.Image(desc.Digest)
			if err != nil {
				return nil, err
			}
			platform = desc.Platform
		}

		commonLayer, err := layer(commonfiles)
		if err != nil {
			return nil, err
		}

		sourceLayer, err := layer(srcfs)
		if err != nil {
			return nil, err
		}

		entrypoint, ok := entrypoints[arch]
		if !ok {
			return nil, fmt.Errorf("unexpected arch %q for entrypoints: %v", arch, maps.Keys(entrypoints))
		}

		entrypointLayer, err := renderEntrypoint(entrypoint)
		if err != nil {
			return nil, err
		}

		img, err := mutate.AppendLayers(baseImg, commonLayer, sourceLayer, entrypointLayer)
		if err != nil {
			return nil, err
		}

		cf, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}

		cf.Config.Entrypoint = []string{"/bin/sh", "/entrypoint.sh"}
		cf.Config.WorkingDir = "/"

		img, err = mutate.ConfigFile(img, cf)
		if err != nil {
			return nil, err
		}

		newDesc, err := partial.Descriptor(img)
		if err != nil {
			return nil, err
		}

		newDesc.Platform = platform

		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: *newDesc,
		})
	}

	return idx, nil
}

// Yuck.
type Graph = map[string]map[string]graph.Edge[string]

type Bundles struct {
	Graph   Graph
	Tasks   []Task
	Runtime name.Digest
}

// TODO: dependency injection
func Pull(pull string) (*Bundles, error) {
	ref, err := name.ParseReference(pull)
	if err != nil {
		return nil, err
	}

	idx, err := remote.Index(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithUserAgent("wolfictl bundle"))
	if err != nil {
		return nil, err
	}

	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}

	if len(im.Manifests) == 0 {
		return nil, fmt.Errorf("no manifests in bundle index: %s", pull)
	}

	bundles := &Bundles{}
	for _, desc := range im.Manifests { //nolint:gocritic
		switch desc.Annotations["dev.wolfi.bundle"] {
		case "graph":
			img, err := idx.Image(desc.Digest)
			if err != nil {
				return nil, err
			}
			layers, err := img.Layers()
			if err != nil {
				return nil, err
			}

			if len(layers) == 0 {
				return nil, fmt.Errorf("no graph layers in entry %s of bundle %s", desc.Digest.String(), pull)
			}

			rc, err := layers[0].Compressed()
			if err != nil {
				return nil, err
			}

			var g Graph
			if err := json.NewDecoder(rc).Decode(&g); err != nil {
				return nil, err
			}

			bundles.Graph = g
		case "tasks":
			img, err := idx.Image(desc.Digest)
			if err != nil {
				return nil, err
			}
			layers, err := img.Layers()
			if err != nil {
				return nil, err
			}

			if len(layers) == 0 {
				return nil, fmt.Errorf("no tasks layers in entry %s of bundle %s", desc.Digest.String(), pull)
			}

			rc, err := layers[0].Compressed()
			if err != nil {
				return nil, err
			}

			var tasks []Task
			if err := json.NewDecoder(rc).Decode(&tasks); err != nil {
				return nil, err
			}

			bundles.Tasks = tasks
		case "runtime":
			bundles.Runtime = ref.Context().Digest(desc.Digest.String())
		}
	}

	return bundles, nil
}

// escapeRFC1123 escapes a string to be RFC1123 compliant.  We don't worry about
// being collision free because these are generally fed to generateName which
// appends a randomized suffix.
func escapeRFC1123(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, ".", "-"), "_", "-"))
}

// Podspec returns bytes of yaml representing a podspec.
// This is a terrible API that we should change.
func Podspec(task Task, ref name.Reference, arch, mFamily, sa, ns string) (*corev1.Pod, error) {
	goarch := types.ParseArchitecture(arch).String()

	// Set some sane default resource requests if none are specified by flag or config.
	// This is required for GKE Autopilot.
	resources := config.Resources{
		CPU:    "2",
		Memory: "4Gi",
	}

	// Copy resources out of the task rather than overwriting in place because both archs share the same task.
	if in := task.Resources; in != nil {
		if in.CPU != "" {
			resources.CPU = in.CPU
		}
		if in.Memory != "" {
			resources.Memory = in.Memory
		}
	}

	cpu, err := strconv.ParseFloat(resources.CPU, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing cpu %q: %w", resources.CPU, err)
	}

	if goarch == "arm64" && cpu > MaxArmCore {
		// Arm machines max out at 48 cores, if a CPU value of greater than 48 is given,
		// set it to 48 to avoid pod being unschedulable.
		// Set to MaxArmCore if greater than MaxArmCore
		cpu = MaxArmCore
	}

	// Reduce cpu by 2% for better bin packing of pods on nodes
	// Example:
	//   64 core machines has 63.77 core available
	//   48 core machines has 47.81 core available
	//   32 core machines has 31.85 core available
	//   16 core machines has 15.89 core available
	cpu *= 0.98

	resources.CPU = fmt.Sprintf("%f", cpu)

	rr := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(resources.CPU),
			corev1.ResourceMemory: resource.MustParse(resources.Memory),
		},
	}

	t := []corev1.Toleration{{
		Effect:   "NoSchedule",
		Key:      "chainguard.dev/runner",
		Operator: "Equal",
		Value:    "bundle-builder",
	}}

	mf := mFamily
	if goarch == "arm64" {
		mf = "t2a"

		t = append(t, corev1.Toleration{
			Effect:   "NoSchedule",
			Key:      "kubernetes.io/arch",
			Operator: "Equal",
			Value:    "arm64",
		})
	}

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-", escapeRFC1123(task.Package), goarch),
			Namespace:    ns,
			Labels: map[string]string{
				"kubernetes.io/arch":             goarch,
				"app.kubernetes.io/component":    task.Package,
				"melange.chainguard.dev/arch":    goarch,
				"melange.chainguard.dev/package": task.Package,
			},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			// Don't putz around for 30s when we kill things.
			TerminationGracePeriodSeconds: ptr.Int64(0),
			Containers: []corev1.Container{{
				Name:  "workspace",
				Image: ref.String(),
				Env: []corev1.EnvVar{{
					Name:  "SOURCE_DATE_EPOCH",
					Value: strconv.FormatInt(task.BuildDateEpoch.Unix(), 10),
				}},
				// TODO: Do we need this??
				// ldconfig is run to prime ld.so.cache for glibc packages which require it.
				// Command:      []string{"/bin/sh", "-c", "[ -x /sbin/ldconfig ] && /sbin/ldconfig /lib || true\nsleep infinity"},
				Args:      []string{task.Path, task.SourceDir},
				Resources: rr,
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "tmp-dir-bundle-builder",
						MountPath: "/tmp",
					},
				},
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.Bool(true),
				},
			}},
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.Bool(false),
			NodeSelector: map[string]string{
				"kubernetes.io/arch": goarch,
			},
			Tolerations:        t,
			ServiceAccountName: sa,
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "tmp-dir-bundle-builder",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}

	if mf != "" {
		// https://cloud.google.com/kubernetes-engine/docs/how-to/node-auto-provisioning#custom_machine_family
		pod.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "cloud.google.com/machine-family",
									Operator: corev1.NodeSelectorOpIn,
									// Warning: Node auto-provisioning doesn't support multiple value assigned to the node affinity. Make sure you assign only one value to the node affinity.
									Values: []string{mf},
								},
							},
						},
					},
				},
			},
		}
	}

	return pod, nil
}

// TODO: Just use tar.Writer.AddFS: https://github.com/golang/go/issues/66831
func tarAddFS(tw *tar.Writer, fsys fs.FS) error {
	return fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// TODO(#49580): Handle symlinks when fs.ReadLinkFS is available.
		if !d.IsDir() && !info.Mode().IsRegular() {
			return errors.New("tar: cannot add non-regular file")
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = name
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := fsys.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

type Task struct {
	Package        string            `json:"package"`
	Version        string            `json:"version"`
	Epoch          uint64            `json:"epoch"`
	Path           string            `json:"path,omitempty"`
	SourceDir      string            `json:"sourceDir,omitempty"`
	Architectures  []string          `json:"architectures,omitempty"`
	Subpackages    []string          `json:"subpackages,omitempty"`
	Resources      *config.Resources `json:"resources,omitempty"`
	BuildDateEpoch time.Time         `json:"buildDateEpoch,omitempty"`
}
