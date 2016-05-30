package openshift

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/golang/glog"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/runtime"
	utilerrs "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/sets"

	deployapi "github.com/openshift/origin/pkg/deploy/api"
	"github.com/openshift/origin/pkg/generate/app"
	"github.com/openshift/origin/pkg/generate/git"
	templateapi "github.com/openshift/origin/pkg/template/api"
	dockerfileutil "github.com/openshift/origin/pkg/util/docker/dockerfile"
	"github.com/openshift/origin/third_party/github.com/docker/libcompose/project"

	// Install OpenShift APIs
	_ "github.com/openshift/origin/pkg/build/api/install"
	_ "github.com/openshift/origin/pkg/deploy/api/install"
	_ "github.com/openshift/origin/pkg/image/api/install"
	_ "github.com/openshift/origin/pkg/route/api/install"
	_ "github.com/openshift/origin/pkg/template/api/install"
)

type TransformData struct {
	errs         []error
	warnings     map[string][]string
	serviceOrder sets.String
	joins        map[string]sets.String
	volumesFrom  map[string][]string
	colocated    []sets.String
	builds       map[string]*app.Pipeline
	pipelines    app.PipelineGroup
	containers   map[string]*kapi.Container
	objects      app.Objects
	bases        []string
	aliases      map[string]sets.String
}

func Transform(p *project.Project, bases []string) error {
	data := TransformData{
		errs:         []error{},
		warnings:     make(map[string][]string),
		serviceOrder: sets.NewString(),
		joins:        make(map[string]sets.String),
		volumesFrom:  make(map[string][]string),
		colocated:    []sets.String{},
		builds:       make(map[string]*app.Pipeline),
		pipelines:    app.PipelineGroup{},
		containers:   make(map[string]*kapi.Container),
		objects:      app.Objects{},
		bases:        bases,
		aliases:      make(map[string]sets.String),
	}

	updateServiceOrder(p, &data)
	updateVolumes(p, &data)

	err := updatePodsList(p, &data)
	if err != nil {
		return err
	}

	updateAliases(p, &data)

	g := app.NewImageRefGenerator()

	err = updateBuildPipeline(p, g, &data)
	if err != nil {
		return err
	}

	fmt.Println("After upateBuildPipeline")
	fmt.Println("pipelines: ", data.pipelines)
	fmt.Println("builds: ", data.builds)

	err = updateDeploymentConfigs(p, g, &data)
	if err != nil {
		return err
	}

	if len(data.errs) > 0 {
		return utilerrs.NewAggregate(data.errs)
	}

	err = updatePipelineObjects(&data)
	if err != nil {
		return err
	}

	err = updateServiceObjects(&data)

	template := &templateapi.Template{}
	template.Name = p.Name

	// for each container that defines VolumesFrom, copy equivalent mounts.
	// TODO: ensure mount names are unique?
	for target, otherContainers := range data.volumesFrom {
		for _, from := range otherContainers {
			for _, volume := range data.containers[from].VolumeMounts {
				data.containers[target].VolumeMounts = append(data.containers[target].VolumeMounts, volume)
			}
		}
	}

	template.Objects = data.objects

	// generate warnings
	if len(data.warnings) > 0 {
		allWarnings := sets.NewString()
		for msg, services := range data.warnings {
			allWarnings.Insert(fmt.Sprintf("%s: %s", strings.Join(services, ","), msg))
		}
		if template.Annotations == nil {
			template.Annotations = make(map[string]string)
		}
		template.Annotations[app.GenerationWarningAnnotation] = fmt.Sprintf("not all docker-compose fields were honored:\n* %s", strings.Join(allWarnings.List(), "\n* "))
	}

	var convErr error
	template.Objects, convErr = convertToVersion(template.Objects, "v1")
	if convErr != nil {
		panic(convErr)
	}

	// make it List instead of Template
	list := &kapi.List{Items: template.Objects}

	printer, _, _err := kubectl.GetPrinter("yaml", "")
	if _err != nil {
		panic(_err)
	}
	version := unversioned.GroupVersion{Group: "", Version: "v1"}
	printer = kubectl.NewVersionedPrinter(printer, kapi.Scheme, version)
	printer.PrintObj(list, os.Stdout)

	return nil
}

// Get ordered services
func updateServiceOrder(p *project.Project, data *TransformData) {
	for k, v := range p.Configs {
		data.serviceOrder.Insert(k)
		warnUnusableComposeElements(k, v, data.warnings)
	}
}

// Update volumes, and joins as well
func updateVolumes(p *project.Project, data *TransformData) {
	for _, k := range data.serviceOrder.List() {
		if data.joins[k] == nil {
			data.joins[k] = sets.NewString(k)
		}
		v := p.Configs[k]
		for _, from := range v.VolumesFrom {
			switch parts := strings.Split(from, ":"); len(parts) {
			case 1:
				data.joins[k].Insert(parts[0])
				data.volumesFrom[k] = append(data.volumesFrom[k], parts[0])
			case 2:
				target := parts[1]
				if parts[1] == "ro" || parts[1] == "rw" {
					target = parts[0]
				}
				data.joins[k].Insert(target)
				data.volumesFrom[k] = append(data.volumesFrom[k], target)
			case 3:
				data.joins[k].Insert(parts[1])
				data.volumesFrom[k] = append(data.volumesFrom[k], parts[1])
			}
		}
	}
}

// Get colocated pods list
func updatePodsList(p *project.Project, data *TransformData) error {
	joinOrder := sets.NewString()
	for k := range data.joins {
		joinOrder.Insert(k)
	}
	for _, k := range joinOrder.List() {
		set := data.joins[k]
		matched := -1
		for i, existing := range data.colocated {
			if set.Intersection(existing).Len() == 0 {
				continue
			}
			if matched != -1 {
				return fmt.Errorf("%q belongs with %v, but %v also contains some overlapping elements", k, set, data.colocated[matched])
			}
			existing.Insert(set.List()...)
			matched = i
			continue
		}
		if matched == -1 {
			data.colocated = append(data.colocated, set)
		}
	}
	return nil
}

// Get service aliases
func updateAliases(p *project.Project, data *TransformData) {
	for _, v := range p.Configs {
		for _, s := range v.Links.Slice() {
			parts := strings.SplitN(s, ":", 2)
			if len(parts) != 2 || parts[0] == parts[1] {
				continue
			}
			set := data.aliases[parts[0]]
			if set == nil {
				set = sets.NewString()
				data.aliases[parts[0]] = set
			}
			set.Insert(parts[1])
		}
	}
}

// find and define build pipelines
func updateBuildPipeline(p *project.Project, g app.ImageRefGenerator, data *TransformData) error {

	for _, k := range data.serviceOrder.List() {
		v := p.Configs[k]
		if len(v.Build) == 0 {
			continue
		}
		if _, ok := data.builds[v.Build]; ok {
			continue
		}
		var base, relative string
		for _, s := range data.bases {
			if !strings.HasPrefix(v.Build, s) {
				continue
			}
			base = s
			path, err := filepath.Rel(base, v.Build)
			if err != nil {
				return fmt.Errorf("path is not relative to base: %v", err)
			}
			relative = path
			break
		}
		if len(base) == 0 {
			return fmt.Errorf("build path outside of the compose file: %s", v.Build)
		}

		// if this is a Git repository, make the path relative
		if root, err := git.NewRepository().GetRootDir(base); err == nil {
			if relative, err = filepath.Rel(root, filepath.Join(base, relative)); err != nil {
				return fmt.Errorf("unable to find relative path for Git repository: %v", err)
			}
			base = root
		}
		buildPath := filepath.Join(base, relative)

		// TODO: what if there is no origin for this repo?

		glog.V(4).Infof("compose service: %#v", v)
		repo, err := app.NewSourceRepositoryWithDockerfile(buildPath, "")
		if err != nil {
			data.errs = append(data.errs, err)
			continue
		}
		repo.BuildWithDocker()

		info := repo.Info()
		if info == nil || info.Dockerfile == nil {
			data.errs = append(data.errs, fmt.Errorf("unable to locate a Dockerfile in %s", v.Build))
			continue
		}
		node := info.Dockerfile.AST()
		baseImage := dockerfileutil.LastBaseImage(node)
		if len(baseImage) == 0 {
			data.errs = append(data.errs, fmt.Errorf("the Dockerfile in the repository %q has no FROM instruction", info.Path))
			continue
		}

		var ports []string
		for _, s := range v.Ports {
			container, _ := extractFirstPorts(s)
			ports = append(ports, container)
		}

		image, err := g.FromNameAndPorts(baseImage, ports)
		if err != nil {
			data.errs = append(data.errs, err)
			continue
		}
		image.AsImageStream = true
		image.TagDirectly = true
		image.ObjectName = k
		image.Tag = "from"

		pipeline, err := app.NewPipelineBuilder(k, nil, false).To(k).NewBuildPipeline(k, image, repo)
		if err != nil {
			data.errs = append(data.errs, err)
			continue
		}
		if len(relative) > 0 {
			pipeline.Build.Source.ContextDir = relative
		}
		// TODO: this should not be necessary
		pipeline.Build.Source.Name = k
		pipeline.Name = k
		pipeline.Image.ObjectName = k
		glog.V(4).Infof("created pipeline %+v", pipeline)

		data.builds[v.Build] = pipeline
		data.pipelines = append(data.pipelines, pipeline)
	}

	if len(data.errs) > 0 {
		return utilerrs.NewAggregate(data.errs)
	}

	return nil
}

func updateDeploymentConfigs(p *project.Project, g app.ImageRefGenerator, data *TransformData) error {

	// create deployment groups
	for _, pod := range data.colocated {
		var group app.PipelineGroup
		commonMounts := make(map[string]string)
		for _, k := range pod.List() {
			v := p.Configs[k]
			glog.V(4).Infof("compose service: %#v", v)
			var inputImage *app.ImageRef
			if len(v.Image) != 0 {
				image, err := g.FromName(v.Image)
				if err != nil {
					data.errs = append(data.errs, err)
					continue
				}
				image.AsImageStream = true
				image.TagDirectly = true
				image.ObjectName = k

				inputImage = image
			}
			if inputImage == nil {
				if previous, ok := data.builds[v.Build]; ok {
					inputImage = previous.Image
				}
			}
			if inputImage == nil {
				data.errs = append(data.errs, fmt.Errorf("could not find an input image for %q", k))
				continue
			}

			inputImage.ContainerFn = func(c *kapi.Container) {
				if len(v.ContainerName) > 0 {
					c.Name = v.ContainerName
				}
				for _, s := range v.Ports {
					container, _ := extractFirstPorts(s)
					if port, err := strconv.Atoi(container); err == nil {
						c.Ports = append(c.Ports, kapi.ContainerPort{ContainerPort: port})
					}
				}
				c.Args = v.Command.Slice()
				if len(v.Entrypoint.Slice()) > 0 {
					c.Command = v.Entrypoint.Slice()
				}
				if len(v.WorkingDir) > 0 {
					c.WorkingDir = v.WorkingDir
				}
				c.Env = append(c.Env, app.ParseEnvironment(v.Environment.Slice()...).List()...)
				if uid, err := strconv.Atoi(v.User); err == nil {
					uid64 := int64(uid)
					if c.SecurityContext == nil {
						c.SecurityContext = &kapi.SecurityContext{}
					}
					c.SecurityContext.RunAsUser = &uid64
				}
				c.TTY = v.Tty
				if v.StdinOpen {
					c.StdinOnce = true
					c.Stdin = true
				}
				if v.Privileged {
					if c.SecurityContext == nil {
						c.SecurityContext = &kapi.SecurityContext{}
					}
					t := true
					c.SecurityContext.Privileged = &t
				}
				if v.ReadOnly {
					if c.SecurityContext == nil {
						c.SecurityContext = &kapi.SecurityContext{}
					}
					t := true
					c.SecurityContext.ReadOnlyRootFilesystem = &t
				}
				if v.MemLimit > 0 {
					q := resource.NewQuantity(v.MemLimit, resource.DecimalSI)
					if c.Resources.Limits == nil {
						c.Resources.Limits = make(kapi.ResourceList)
					}
					c.Resources.Limits[kapi.ResourceMemory] = *q
				}

				if quota := v.CPUQuota; quota > 0 {
					if quota < 1000 {
						quota = 1000 // minQuotaPeriod
					}
					milliCPU := quota * 1000     // milliCPUtoCPU
					milliCPU = milliCPU / 100000 // quotaPeriod
					q := resource.NewMilliQuantity(milliCPU, resource.DecimalSI)
					if c.Resources.Limits == nil {
						c.Resources.Limits = make(kapi.ResourceList)
					}
					c.Resources.Limits[kapi.ResourceCPU] = *q
				}
				if shares := v.CPUShares; shares > 0 {
					if shares < 2 {
						shares = 2 // minShares
					}
					milliCPU := shares * 1000  // milliCPUtoCPU
					milliCPU = milliCPU / 1024 // sharesPerCPU
					q := resource.NewMilliQuantity(milliCPU, resource.DecimalSI)
					if c.Resources.Requests == nil {
						c.Resources.Requests = make(kapi.ResourceList)
					}
					c.Resources.Requests[kapi.ResourceCPU] = *q
				}

				mountPoints := make(map[string][]string)
				for _, s := range v.Volumes {
					switch parts := strings.SplitN(s, ":", 3); len(parts) {
					case 1:
						mountPoints[""] = append(mountPoints[""], parts[0])

					case 2:
						fallthrough
					default:
						mountPoints[parts[0]] = append(mountPoints[parts[0]], parts[1])
					}
				}
				for from, at := range mountPoints {
					name, ok := commonMounts[from]
					if !ok {
						name = fmt.Sprintf("dir-%d", len(commonMounts)+1)
						commonMounts[from] = name
					}
					for _, path := range at {
						c.VolumeMounts = append(c.VolumeMounts, kapi.VolumeMount{Name: name, MountPath: path})
					}
				}
			}

			pipeline, err := app.NewPipelineBuilder(k, nil, true).To(k).NewImagePipeline(k, inputImage)
			if err != nil {
				data.errs = append(data.errs, err)
				break
			}

			if err := pipeline.NeedsDeployment(nil, nil, false); err != nil {
				return err
			}

			group = append(group, pipeline)
		}
		if err := group.Reduce(); err != nil {
			return err
		}
		data.pipelines = append(data.pipelines, group...)
	}
	return nil
}

func updatePipelineObjects(data *TransformData) error {
	acceptors := app.Acceptors{app.NewAcceptUnique(kapi.Scheme), app.AcceptNew}
	accept := app.NewAcceptFirst()
	for _, p := range data.pipelines {
		accepted, err := p.Objects(accept, acceptors)
		if err != nil {
			return fmt.Errorf("can't setup %q: %v", p.From, err)
		}
		data.objects = append(data.objects, accepted...)
	}
	return nil
}

func updateServiceObjects(data *TransformData) error {

	// create services for each object with a name based on alias.
	var services []*kapi.Service
	for _, obj := range data.objects {
		switch t := obj.(type) {
		case *deployapi.DeploymentConfig:
			ports := app.UniqueContainerToServicePorts(app.AllContainerPorts(t.Spec.Template.Spec.Containers...))
			if len(ports) == 0 {
				continue
			}
			svc := app.GenerateService(t.ObjectMeta, t.Spec.Selector)
			if data.aliases[svc.Name].Len() == 1 {
				svc.Name = data.aliases[svc.Name].List()[0]
			}
			svc.Spec.Ports = ports
			services = append(services, svc)

			// take a reference to each container
			for i := range t.Spec.Template.Spec.Containers {
				c := &t.Spec.Template.Spec.Containers[i]
				data.containers[c.Name] = c
			}
		}
	}
	for _, svc := range services {
		data.objects = append(data.objects, svc)
	}

	return nil
}

// extractFirstPorts converts a Docker compose port spec (CONTAINER, HOST:CONTAINER, or
// IP:HOST:CONTAINER) to the first container and host port in the range.  Host port will
// default to container port.
func extractFirstPorts(port string) (container, host string) {
	segments := strings.Split(port, ":")
	container = segments[len(segments)-1]
	container = rangeToPort(container)
	switch {
	case len(segments) == 3:
		host = rangeToPort(segments[1])
	case len(segments) == 2 && net.ParseIP(segments[0]) == nil:
		host = rangeToPort(segments[0])
	default:
		host = container
	}
	return container, host
}

func rangeToPort(s string) string {
	parts := strings.SplitN(s, "-", 2)
	return parts[0]
}

// warnUnusableComposeElements add warnings for unsupported elements in the provided service config
func warnUnusableComposeElements(k string, v *project.ServiceConfig, warnings map[string][]string) {
	fn := func(msg string) {
		warnings[msg] = append(warnings[msg], k)
	}
	if len(v.CapAdd) > 0 || len(v.CapDrop) > 0 {
		// TODO: we can support this
		fn("cap_add and cap_drop are not supported")
	}
	if len(v.CgroupParent) > 0 {
		fn("cgroup_parent is not supported")
	}
	if len(v.CPUSet) > 0 {
		fn("cpuset is not supported")
	}
	if len(v.Devices) > 0 {
		fn("devices are not supported")
	}
	if v.DNS.Len() > 0 || v.DNSSearch.Len() > 0 {
		fn("dns and dns_search are not supported")
	}
	if len(v.DomainName) > 0 {
		fn("domainname is not supported")
	}
	if len(v.Hostname) > 0 {
		fn("hostname is not supported")
	}
	if len(v.Labels.MapParts()) > 0 {
		fn("labels is ignored")
	}
	if len(v.Links.Slice()) > 0 {
		//fn("links are not supported, use services to talk to other pods")
		// TODO: display some sort of warning when linking will be inconsistent
	}
	if len(v.LogDriver) > 0 {
		fn("log_driver is not supported")
	}
	if len(v.MacAddress) > 0 {
		fn("mac_address is not supported")
	}
	if len(v.Net) > 0 {
		fn("net is not supported")
	}
	if len(v.Pid) > 0 {
		fn("pid is not supported")
	}
	if len(v.Uts) > 0 {
		fn("uts is not supported")
	}
	if len(v.Ipc) > 0 {
		fn("ipc is not supported")
	}
	if v.MemSwapLimit > 0 {
		fn("mem_swap_limit is not supported")
	}
	if len(v.Restart) > 0 {
		fn("restart is ignored - all pods are automatically restarted")
	}
	if len(v.SecurityOpt) > 0 {
		fn("security_opt is not supported")
	}
	if len(v.User) > 0 {
		if _, err := strconv.Atoi(v.User); err != nil {
			fn("setting user to a string is not supported - use numeric user value")
		}
	}
	if len(v.VolumeDriver) > 0 {
		fn("volume_driver is not supported")
	}
	if len(v.VolumesFrom) > 0 {
		fn("volumes_from is not supported")
		// TODO: use volumes from for colocated containers to automount volumes
	}
	if len(v.ExternalLinks) > 0 {
		fn("external_links are not supported - use services")
	}
	if len(v.LogOpt) > 0 {
		fn("log_opt is not supported")
	}
	if len(v.ExtraHosts) > 0 {
		fn("extra_hosts is not supported")
	}
	if len(v.Ulimits.Elements) > 0 {
		fn("ulimits is not supported")
	}
	// TODO: fields to handle
	// EnvFile       Stringorslice     `yaml:"env_file,omitempty"`
}

func convertToVersion(objs []runtime.Object, version string) ([]runtime.Object, error) {
	ret := []runtime.Object{}

	for _, obj := range objs {

		convertedObject, err := kapi.Scheme.ConvertToVersion(obj, version)
		if err != nil {
			return nil, err
		}

		ret = append(ret, convertedObject)
	}

	return ret, nil
}