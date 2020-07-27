package operator

import (
	"context"
	"crypto/rand"
	"math/big"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/pointer"
)

const (
	baremetalConfigmap    = "metal3-config"
	baremetalSharedVolume = "metal3-shared"
	baremetalSecretName   = "metal3-mariadb-password"
	baremetalSecretKey    = "password"
	ironicSecretName      = "metal3-ironic-password"
	ironicSecretKey       = "password"
	ironicUsername        = "metal3"
)

var volumes = []corev1.Volume{
	{
		Name: baremetalSharedVolume,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	},
}

var volumeMounts = []corev1.VolumeMount{
	{
		Name:      baremetalSharedVolume,
		MountPath: "/shared",
	},
}

func buildEnvVar(name string, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.EnvVar {
	value := getMetal3DeploymentConfig(name, baremetalProvisioningConfig)
	if value != nil {
		return corev1.EnvVar{
			Name:  name,
			Value: *value,
		}
	} else {
		return corev1.EnvVar{
			Name: name,
		}
	}
}

func setMariadbPassword() corev1.EnvVar {
	return corev1.EnvVar{
		Name: "MARIADB_PASSWORD",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: baremetalSecretName,
				},
				Key: baremetalSecretKey,
			},
		},
	}
}

func setIronicPassword(name string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: ironicSecretName,
				},
				Key: ironicSecretKey,
			},
		},
	}
}

func generateRandomPassword() (string, error) {
	chars := []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz" +
		"0123456789")
	length := 16
	buf := make([]rune, length)
	numChars := big.NewInt(int64(len(chars)))
	for i := range buf {
		c, err := rand.Int(rand.Reader, numChars)
		if err != nil {
			return "", err
		}
		buf[i] = chars[c.Uint64()]
	}
	return string(buf), nil
}

func createMariadbPasswordSecret(client coreclientv1.SecretsGetter, config *OperatorConfig) error {
	glog.V(3).Info("Checking if the MariaDB password secret already exists")
	_, err := client.Secrets(config.TargetNamespace).Get(context.Background(), baremetalSecretName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Secret does not already exist. So, create one.
	password, err := generateRandomPassword()
	if err != nil {
		return err
	}
	_, err = client.Secrets(config.TargetNamespace).Create(
		context.Background(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      baremetalSecretName,
				Namespace: config.TargetNamespace,
			},
			StringData: map[string]string{
				baremetalSecretKey: password,
			},
		},
		metav1.CreateOptions{},
	)
	return err
}

func createIronicPasswordSecret(client coreclientv1.SecretsGetter, config *OperatorConfig) error {
	glog.V(3).Info("Checking if the Ironic password secret already exists")
	_, err := client.Secrets(config.TargetNamespace).Get(context.Background(), ironicSecretName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Secret does not already exist. So, create one.
	password, err := generateRandomPassword()
	if err != nil {
		return err
	}

	_, err = client.Secrets(config.TargetNamespace).Create(
		context.Background(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ironicSecretName,
				Namespace: config.TargetNamespace,
			},
			StringData: map[string]string{
				ironicSecretKey: password,
			},
		},
		metav1.CreateOptions{},
	)
	return err
}

func createMetal3PasswordSecrets(client coreclientv1.SecretsGetter, config *OperatorConfig) error {
	if err := createMariadbPasswordSecret(client, config); err != nil {
		glog.Error("Failed to create Mariadb password.")
		return err
	}
	if err := createIronicPasswordSecret(client, config); err != nil {
		glog.Error("Failed to create Ironic password.")
		return err
	}
	return nil
}

func newMetal3Deployment(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) *appsv1.Deployment {
	replicas := int32(1)
	template := newMetal3PodTemplateSpec(config, baremetalProvisioningConfig)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "metal3",
			Namespace: config.TargetNamespace,
			Annotations: map[string]string{
				maoOwnedAnnotation: "",
			},
			Labels: map[string]string{
				"api":     "clusterapi",
				"k8s-app": "controller",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"api":     "clusterapi",
					"k8s-app": "controller",
				},
			},
			Template: *template,
		},
	}
}

func newMetal3PodTemplateSpec(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) *corev1.PodTemplateSpec {
	initContainers := newMetal3InitContainers(config, baremetalProvisioningConfig)
	containers := newMetal3Containers(config, baremetalProvisioningConfig)
	tolerations := []corev1.Toleration{
		{
			Key:    "node-role.kubernetes.io/master",
			Effect: corev1.TaintEffectNoSchedule,
		},
		{
			Key:      "CriticalAddonsOnly",
			Operator: corev1.TolerationOpExists,
		},
		{
			Key:               "node.kubernetes.io/not-ready",
			Effect:            corev1.TaintEffectNoExecute,
			Operator:          corev1.TolerationOpExists,
			TolerationSeconds: pointer.Int64Ptr(120),
		},
		{
			Key:               "node.kubernetes.io/unreachable",
			Effect:            corev1.TaintEffectNoExecute,
			Operator:          corev1.TolerationOpExists,
			TolerationSeconds: pointer.Int64Ptr(120),
		},
	}

	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"api":     "clusterapi",
				"k8s-app": "controller",
			},
		},
		Spec: corev1.PodSpec{
			Volumes:           volumes,
			InitContainers:    initContainers,
			Containers:        containers,
			HostNetwork:       true,
			PriorityClassName: "system-node-critical",
			NodeSelector:      map[string]string{"node-role.kubernetes.io/master": ""},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: pointer.BoolPtr(false),
			},
			ServiceAccountName: "machine-api-controllers",
			Tolerations:        tolerations,
		},
	}
}

func newMetal3InitContainers(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) []corev1.Container {
	initContainers := []corev1.Container{
		{
			Name:            "metal3-ipa-downloader",
			Image:           config.BaremetalControllers.IronicIpaDownloader,
			Command:         []string{"/usr/local/bin/get-resource.sh"},
			ImagePullPolicy: "IfNotPresent",
			SecurityContext: &corev1.SecurityContext{
				Privileged: pointer.BoolPtr(true),
			},
			VolumeMounts: volumeMounts,
			Env:          []corev1.EnvVar{},
		},
	}
	initContainers = append(initContainers, createInitContainerMachineOsDownloader(config, baremetalProvisioningConfig))
	initContainers = append(initContainers, createInitContainerStaticIpSet(config, baremetalProvisioningConfig))
	return initContainers
}

func createInitContainerMachineOsDownloader(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {
	initContainer := corev1.Container{
		Name:            "metal3-machine-os-downloader",
		Image:           config.BaremetalControllers.IronicMachineOsDownloader,
		Command:         []string{"/usr/local/bin/get-resource.sh"},
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			buildEnvVar("RHCOS_IMAGE_URL", baremetalProvisioningConfig),
		},
	}
	return initContainer
}

func createInitContainerStaticIpSet(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {
	initContainer := corev1.Container{
		Name:            "metal3-static-ip-set",
		Image:           config.BaremetalControllers.IronicStaticIpManager,
		Command:         []string{"/set-static-ip"},
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		Env: []corev1.EnvVar{
			buildEnvVar("PROVISIONING_IP", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_INTERFACE", baremetalProvisioningConfig),
		},
	}
	return initContainer
}

func newMetal3Containers(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) []corev1.Container {
	//Starting off with the metal3-baremetal-operator container
	containers := []corev1.Container{
		{
			Name:  "metal3-baremetal-operator",
			Image: config.BaremetalControllers.BaremetalOperator,
			Ports: []corev1.ContainerPort{
				{
					Name:          "metrics",
					ContainerPort: 60000,
				},
			},
			Command:         []string{"/baremetal-operator"},
			ImagePullPolicy: "IfNotPresent",
			Env: []corev1.EnvVar{
				{
					Name: "WATCH_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.namespace",
						},
					},
				},
				{
					Name: "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.name",
						},
					},
				},
				{
					Name:  "OPERATOR_NAME",
					Value: "baremetal-operator",
				},
				buildEnvVar("DEPLOY_KERNEL_URL", baremetalProvisioningConfig),
				buildEnvVar("DEPLOY_RAMDISK_URL", baremetalProvisioningConfig),
				buildEnvVar("IRONIC_ENDPOINT", baremetalProvisioningConfig),
				buildEnvVar("IRONIC_INSPECTOR_ENDPOINT", baremetalProvisioningConfig),
				{
					Name:  "IRONIC_AUTH_STRATEGY",
					Value: "http_basic",
				},
				{
					Name:  "IRONIC_HTTP_BASIC_USERNAME",
					Value: ironicUsername,
				},
				setIronicPassword("IRONIC_HTTP_BASIC_PASSWORD"),
				{
					Name:  "INSPECTOR_HTTP_BASIC_USERNAME",
					Value: ironicUsername,
				},
				setIronicPassword("INSPECTOR_HTTP_BASIC_PASSWORD"),
			},
		},
	}
	if baremetalProvisioningConfig.ProvisioningNetwork != provisioningNetworkDisabled {
		containers = append(containers, createContainerMetal3Dnsmasq(config, baremetalProvisioningConfig))
	}
	containers = append(containers, createContainerMetal3Mariadb(config))
	containers = append(containers, createContainerMetal3Httpd(config, baremetalProvisioningConfig))
	containers = append(containers, createContainerMetal3IronicConductor(config, baremetalProvisioningConfig))
	containers = append(containers, createContainerMetal3IronicApi(config, baremetalProvisioningConfig))
	containers = append(containers, createContainerMetal3IronicInspector(config, baremetalProvisioningConfig))
	containers = append(containers, createContainerMetal3StaticIpManager(config, baremetalProvisioningConfig))
	return containers
}

func createContainerMetal3Dnsmasq(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {

	container := corev1.Container{
		Name:            "metal3-dnsmasq",
		Image:           config.BaremetalControllers.Ironic,
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		Command:      []string{"/bin/rundnsmasq"},
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			buildEnvVar("HTTP_PORT", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_INTERFACE", baremetalProvisioningConfig),
			buildEnvVar("DHCP_RANGE", baremetalProvisioningConfig),
		},
	}
	return container
}

func createContainerMetal3Mariadb(config *OperatorConfig) corev1.Container {

	container := corev1.Container{
		Name:            "metal3-mariadb",
		Image:           config.BaremetalControllers.Ironic,
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		Command:      []string{"/bin/runmariadb"},
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			setMariadbPassword(),
		},
	}
	return container
}

func createContainerMetal3Httpd(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {

	container := corev1.Container{
		Name:            "metal3-httpd",
		Image:           config.BaremetalControllers.Ironic,
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		Command:      []string{"/bin/runhttpd"},
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			buildEnvVar("HTTP_PORT", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_IP", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_INTERFACE", baremetalProvisioningConfig),
		},
	}
	return container
}

func createContainerMetal3IronicConductor(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {

	container := corev1.Container{
		Name:            "metal3-ironic-conductor",
		Image:           config.BaremetalControllers.Ironic,
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		Command:      []string{"/bin/runironic-conductor"},
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			setMariadbPassword(),
			buildEnvVar("HTTP_PORT", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_IP", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_INTERFACE", baremetalProvisioningConfig),
			{
				Name:  "USE_HTTP_BASIC",
				Value: "true",
			},
			{
				Name:  "IRONIC_HTTP_BASIC_USERNAME",
				Value: ironicUsername,
			},
			setIronicPassword("IRONIC_HTTP_BASIC_PASSWORD"),
			{
				Name:  "INSPECTOR_HTTP_BASIC_USERNAME",
				Value: ironicUsername,
			},
			setIronicPassword("INSPECTOR_HTTP_BASIC_PASSWORD"),
		},
	}
	return container
}

func createContainerMetal3IronicApi(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {

	container := corev1.Container{
		Name:            "metal3-ironic-api",
		Image:           config.BaremetalControllers.Ironic,
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		Command:      []string{"/bin/runironic-api"},
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			setMariadbPassword(),
			buildEnvVar("HTTP_PORT", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_IP", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_INTERFACE", baremetalProvisioningConfig),
			{
				Name:  "USE_HTTP_BASIC",
				Value: "true",
			},
			{
				Name:  "IRONIC_HTTP_BASIC_USERNAME",
				Value: ironicUsername,
			},
			setIronicPassword("IRONIC_HTTP_BASIC_PASSWORD"),
		},
	}
	return container
}

func createContainerMetal3IronicInspector(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {

	container := corev1.Container{
		Name:            "metal3-ironic-inspector",
		Image:           config.BaremetalControllers.IronicInspector,
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			buildEnvVar("PROVISIONING_IP", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_INTERFACE", baremetalProvisioningConfig),
			{
				Name:  "USE_HTTP_BASIC",
				Value: "true",
			},
			{
				Name:  "INSPECTOR_HTTP_BASIC_USERNAME",
				Value: ironicUsername,
			},
			setIronicPassword("INSPECTOR_HTTP_BASIC_PASSWORD"),
			{
				Name:  "IRONIC_HTTP_BASIC_USERNAME",
				Value: ironicUsername,
			},
			setIronicPassword("IRONIC_HTTP_BASIC_PASSWORD"),
		},
	}
	return container
}

func createContainerMetal3StaticIpManager(config *OperatorConfig, baremetalProvisioningConfig BaremetalProvisioningConfig) corev1.Container {

	container := corev1.Container{
		Name:            "metal3-static-ip-manager",
		Image:           config.BaremetalControllers.IronicStaticIpManager,
		Command:         []string{"/refresh-static-ip"},
		ImagePullPolicy: "IfNotPresent",
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		},
		Env: []corev1.EnvVar{
			buildEnvVar("PROVISIONING_IP", baremetalProvisioningConfig),
			buildEnvVar("PROVISIONING_INTERFACE", baremetalProvisioningConfig),
		},
	}
	return container
}
