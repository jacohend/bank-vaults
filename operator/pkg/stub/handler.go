package stub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jacohend/bank-vaults/operator/pkg/apis/vault/v1alpha1"
	"github.com/jacohend/bank-vaults/pkg/kv/k8s"
	"github.com/jacohend/bank-vaults/pkg/tls"
	"github.com/jacohend/bank-vaults/pkg/vault"
	etcdV1beta2 "github.com/coreos/etcd-operator/pkg/apis/etcd/v1beta2"
	"github.com/coreos/etcd-operator/pkg/util/etcdutil"
	"github.com/hashicorp/vault/api"
	"github.com/operator-framework/operator-sdk/pkg/sdk/action"
	"github.com/operator-framework/operator-sdk/pkg/sdk/handler"
	"github.com/operator-framework/operator-sdk/pkg/sdk/query"
	"github.com/operator-framework/operator-sdk/pkg/sdk/types"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NewHandler returns a new Vault operator event handler
func NewHandler() handler.Handler {
	return &Handler{}
}

// Handler is a Vault operator event handler
type Handler struct {
}

// Handle handles Vault operator events
func (h *Handler) Handle(ctx types.Context, event types.Event) error {
	switch o := event.Object.(type) {
	case *v1alpha1.Vault:
		v := o

		// Ignore the delete event since the garbage collector will clean up all secondary resources for the CR
		// All secondary resources must have the CR set as their OwnerReference for this to be the case
		if event.Deleted {
			return nil
		}

		// check if we need to create an etcd cluster
		if v.Spec.GetStorageType() == "etcd" {

			etcdCluster, err := etcdForVault(v)
			if err != nil {
				return fmt.Errorf("failed to fabricate etcd cluster: %v", err)
			}

			// Create the secret if it doesn't exist
			sec, err := secretForEtcd(etcdCluster)
			if err != nil {
				return fmt.Errorf("failed to fabricate secret for etcd: %v", err)
			}

			addOwnerRefToObject(sec, asOwner(v))

			err = action.Create(sec)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create secret for etcd: %v", err)
			}

			err = action.Create(etcdCluster)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create etcd cluster: %v", err)
			}
		}

		// Create the secret if it doesn't exist
		sec, err := secretForVault(v)
		if err != nil {
			return fmt.Errorf("failed to fabricate secret for vault: %v", err)
		}

		addOwnerRefToObject(sec, asOwner(v))

		err = action.Create(sec)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create secret for vault: %v", err)
		}

		// Create the deployment if it doesn't exist
		dep, err := deploymentForVault(v)
		if err != nil {
			return fmt.Errorf("failed to fabricate deployment: %v", err)
		}
		err = action.Create(dep)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create deployment: %v", err)
		}
		logDeployment(dep)

		// Ensure the deployment size is the same as the spec
		err = query.Get(dep)
		if err != nil {
			return fmt.Errorf("failed to get deployment: %v", err)
		}
		size := v.Spec.Size
		if *dep.Spec.Replicas != size {
			dep.Spec.Replicas = &size
			err = action.Update(dep)
			if err != nil {
				return fmt.Errorf("failed to update deployment: %v", err)
			}
		}

		// Update the Vault status with the pod names
		podList := podList()
		labelSelector := labels.SelectorFromSet(labelsForVault(v.Name)).String()
		listOps := &metav1.ListOptions{LabelSelector: labelSelector}
		err = query.List(v.Namespace, podList, query.WithListOptions(listOps))
		if err != nil {
			return fmt.Errorf("failed to list pods: %v", err)
		}
		podNames := getPodNames(podList.Items)
		if !reflect.DeepEqual(podNames, v.Status.Nodes) {
			v.Status.Nodes = podNames
			err := action.Update(v)
			if err != nil {
				return fmt.Errorf("failed to update vault status: %v", err)
			}
		}

		// Create the service if it doesn't exist
		ser := serviceForVault(v)
		err = action.Create(ser)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create service: %v", err)
		}

		// Create the deployment if it doesn't exist
		dep = deploymentForConfigurer(v)
		err = action.Create(dep)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create configurer deployment: %v", err)
		}
		logDeployment(dep)

		// Create the configmap if it doesn't exist
		cm := configMapForConfigurer(v)
		err = action.Create(cm)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create configurer configmap: %v", err)
		}

		// Ensure the configmap is the same as the spec
		err = query.Get(cm)
		if err != nil {
			return fmt.Errorf("failed to get deployment: %v", err)
		}

		externalConfig := v.Spec.ExternalConfigJSON()
		if cm.Data[vault.DefaultConfigFile] != externalConfig {
			cm.Data[vault.DefaultConfigFile] = externalConfig
			err = action.Update(cm)
			if err != nil {
				return fmt.Errorf("failed to update configurer configmap: %v", err)
			}
		}

	}
	return nil
}

func logDeployment(dep *appsv1.Deployment) error {
	data, err := json.Marshal(dep)
	if err != nil {
		return fmt.Errorf("failed to marshal the deployment object: %v", err)
	}
	var prettyData bytes.Buffer
	err = json.Indent(&prettyData, data, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to indent the content: %v", err)
	}

	logrus.Infoln("Deployed:")
	if logrus.GetLevel() >= logrus.InfoLevel {
		// use println because the logrus formatter is messing up the JSON indet
		fmt.Println(string(prettyData.Bytes()))
	}
	return nil
}

func secretForEtcd(e *etcdV1beta2.EtcdCluster) (*v1.Secret, error) {
	hosts := []string{
		e.Name,
		e.Name + "." + e.Namespace,
		"*." + e.Name + "." + e.Namespace + ".svc",
		e.Name + "-client." + e.Namespace + ".svc",
		"localhost",
	}
	chain, err := tls.GenerateTLS(strings.Join(hosts, ","), "8760h")
	if err != nil {
		return nil, err
	}

	secret := &v1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
	}
	secret.Name = e.Name + "-tls"
	secret.Namespace = e.Namespace
	secret.Labels = labelsForVault(e.Name)
	secret.StringData = map[string]string{}

	secret.StringData[etcdutil.CliCAFile] = chain.CACert
	secret.StringData[etcdutil.CliCertFile] = chain.ClientCert
	secret.StringData[etcdutil.CliKeyFile] = chain.ClientKey

	secret.StringData["peer-ca.crt"] = chain.CACert
	secret.StringData["peer.crt"] = chain.PeerCert
	secret.StringData["peer.key"] = chain.PeerKey

	secret.StringData["server-ca.crt"] = chain.CACert
	secret.StringData["server.crt"] = chain.ServerCert
	secret.StringData["server.key"] = chain.ServerKey

	return secret, nil
}

func secretForVault(om *v1alpha1.Vault) (*v1.Secret, error) {
	hostsAndIPs := om.Name + "." + om.Namespace + ",127.0.0.1"
	chain, err := tls.GenerateTLS(hostsAndIPs, "8760h")
	if err != nil {
		return nil, err
	}

	secret := &v1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
	}
	secret.Name = om.Name + "-tls"
	secret.Namespace = om.Namespace
	secret.Labels = labelsForVault(om.Name)
	secret.StringData = map[string]string{}
	secret.StringData["ca.crt"] = chain.CACert
	secret.StringData["server.crt"] = chain.ServerCert
	secret.StringData["server.key"] = chain.ServerKey
	return secret, nil
}

// deploymentForVault returns a vault Deployment object
func deploymentForVault(v *v1alpha1.Vault) (*appsv1.Deployment, error) {
	ls := labelsForVault(v.Name)
	replicas := v.Spec.Size

	// validate configuration
	if replicas > 1 && !v.Spec.HasHAStorage() {
		return nil, fmt.Errorf("More than 1 replicas are not supported without HA storage backend")
	}

	volumes := withCredentialsVolume(v, []v1.Volume{
		{
			Name: "vault-config",
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "vault-file",
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "vault-tls",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: v.Name + "-tls",
				},
			},
		},
	})

	volumeMounts := withCredentialsVolumeMount(v, []v1.VolumeMount{
		{
			Name:      "vault-config",
			MountPath: "/vault/config",
		}, {
			Name:      "vault-file",
			MountPath: "/vault/file",
		}, {
			Name:      "vault-tls",
			MountPath: "/vault/tls",
		},
	})

	// TODO Configure Vault to wait for etcd in an init container in this case
	if v.Spec.GetStorageType() == "etcd" {

		// Overwrite Vault config with the generated TLS certificate's settings
		etcdStorage := v.Spec.GetStorage()
		etcdStorage["tls_ca_file"] = "/etcd/tls/" + etcdutil.CliCAFile
		etcdStorage["tls_cert_file"] = "/etcd/tls/" + etcdutil.CliCertFile
		etcdStorage["tls_key_file"] = "/etcd/tls/" + etcdutil.CliKeyFile

		// Mount the Secret holding the certificate into Vault
		etcdAddress := etcdStorage["address"].(string)
		etcdURL, err := url.Parse(etcdAddress)
		if err != nil {
			return nil, err
		}
		etcdName := etcdURL.Hostname()

		etcdVolume := v1.Volume{
			Name: "etcd-tls",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: etcdName + "-tls",
				},
			},
		}

		volumes = append(volumes, etcdVolume)

		etcdVolumeMount := v1.VolumeMount{
			Name:      "etcd-tls",
			MountPath: "/etcd/tls",
		}
		volumeMounts = append(volumeMounts, etcdVolumeMount)
	}

	configJSON := v.Spec.ConfigJSON()
	owner := asOwner(v)
	ownerJSON, err := json.Marshal(owner)
	if err != nil {
		return nil, err
	}

	// TODO check if upgrade strategy is HA
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      v.Name,
			Namespace: v.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Image:           v.Spec.Image,
							ImagePullPolicy: v1.PullIfNotPresent,
							Name:            "vault",
							Args:            []string{"server", "-log-level=debug"},
							Ports: []v1.ContainerPort{{
								ContainerPort: 8200,
								Name:          "vault",
							}},
							Env: withCredentialsEnv(v, []v1.EnvVar{
								{
									Name:  "VAULT_LOCAL_CONFIG",
									Value: configJSON,
								}, {
									Name:  api.EnvVaultCACert,
									Value: "/vault/tls/ca.crt",
								},
							}),
							SecurityContext: &v1.SecurityContext{
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{"IPC_LOCK"},
								},
							},
							// This probe makes sure Vault is responsive in a HTTPS manner
							// See: https://www.vaultproject.io/api/system/init.html
							LivenessProbe: &v1.Probe{
								Handler: v1.Handler{
									HTTPGet: &v1.HTTPGetAction{
										Scheme: v1.URISchemeHTTPS,
										Port:   intstr.FromString("vault"),
										Path:   "/v1/sys/init",
									}},
							},
							// This probe makes sure that only the active Vault instance gets traffic
							// See: https://www.vaultproject.io/api/system/health.html
							ReadinessProbe: &v1.Probe{
								Handler: v1.Handler{
									HTTPGet: &v1.HTTPGetAction{
										Scheme: v1.URISchemeHTTPS,
										Port:   intstr.FromString("vault"),
										Path:   "/v1/sys/health",
									}},
							},
							VolumeMounts: volumeMounts,
						},
						{
							Image:           v.Spec.GetBankVaultsImage(),
							ImagePullPolicy: v1.PullIfNotPresent,
							Name:            "bank-vaults",
							Command:         []string{"bank-vaults", "unseal", "--init"},
							Args:            v.Spec.UnsealConfig.ToArgs(v),
							Env: withCredentialsEnv(v, []v1.EnvVar{
								{
									Name:  k8s.EnvK8SOwnerReference,
									Value: string(ownerJSON),
								}, {
									Name:  api.EnvVaultCACert,
									Value: "/vault/tls/ca.crt",
								},
							}),
							VolumeMounts: withCredentialsVolumeMount(v, []v1.VolumeMount{{
								Name:      "vault-tls",
								MountPath: "/vault/tls",
							}}),
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
	addOwnerRefToObject(dep, owner)
	return dep, nil
}

func withCredentialsEnv(v *v1alpha1.Vault, envs []v1.EnvVar) []v1.EnvVar {
	env := v.Spec.CredentialsConfig.Env
	path := v.Spec.CredentialsConfig.Path
	if env != "" {
		envs = append(envs, v1.EnvVar{
			Name:  env,
			Value: path,
		})
	}
	return envs
}

func withCredentialsVolume(v *v1alpha1.Vault, volumes []v1.Volume) []v1.Volume {
	secretName := v.Spec.CredentialsConfig.SecretName
	if secretName != "" {
		volumes = append(volumes, v1.Volume{
			Name: secretName,
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: secretName,
				},
			},
		})
	}
	return volumes
}

func withCredentialsVolumeMount(v *v1alpha1.Vault, volumeMounts []v1.VolumeMount) []v1.VolumeMount {
	secretName := v.Spec.CredentialsConfig.SecretName
	path := v.Spec.CredentialsConfig.Path
	if secretName != "" {
		_, file := filepath.Split(path)
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			Name:      secretName,
			MountPath: path,
			SubPath:   file,
		})
	}
	return volumeMounts
}

func etcdForVault(v *v1alpha1.Vault) (*etcdV1beta2.EtcdCluster, error) {
	storage := v.Spec.GetStorage()
	etcdAddress := storage["address"].(string)
	etcdURL, err := url.Parse(etcdAddress)
	if err != nil {
		return nil, err
	}
	etcdName := etcdURL.Hostname()
	etcdCluster := &etcdV1beta2.EtcdCluster{}
	etcdCluster.APIVersion = etcdV1beta2.SchemeGroupVersion.String()
	etcdCluster.Kind = etcdV1beta2.EtcdClusterResourceKind
	etcdCluster.Name = etcdName
	etcdCluster.Namespace = v.Namespace
	etcdCluster.Labels = labelsForVault(v.Name)
	etcdCluster.Spec.Size = 3
	// See https://github.com/coreos/etcd-operator/issues/1962#issuecomment-390539621
	// for more details why we have to pin to 3.1.15
	etcdCluster.Spec.Version = "3.1.15"
	etcdCluster.Spec.TLS = &etcdV1beta2.TLSPolicy{
		Static: &etcdV1beta2.StaticTLS{
			OperatorSecret: etcdName + "-tls",
			Member: &etcdV1beta2.MemberSecret{
				ServerSecret: etcdName + "-tls",
				PeerSecret:   etcdName + "-tls",
			},
		},
	}

	addOwnerRefToObject(etcdCluster, asOwner(v))

	return etcdCluster, nil
}

func serviceForVault(v *v1alpha1.Vault) *v1.Service {
	ls := labelsForVault(v.Name)
	service := &v1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      v.Name,
			Namespace: v.Namespace,
		},
		Spec: v1.ServiceSpec{
			Type:     v1.ServiceTypeNodePort,
			Selector: ls,
			Ports: []v1.ServicePort{
				{
					Name: "vault",
					Port: 8200,
				},
			},
		},
	}
	addOwnerRefToObject(service, asOwner(v))
	return service
}

func deploymentForConfigurer(v *v1alpha1.Vault) *appsv1.Deployment {
	ls := labelsForVaultConfigurer(v.Name)
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      v.Name + "-configurer",
			Namespace: v.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Image:           v.Spec.GetBankVaultsImage(),
							ImagePullPolicy: v1.PullIfNotPresent,
							Name:            "bank-vaults",
							Command:         []string{"bank-vaults", "configure"},
							Args:            v.Spec.UnsealConfig.ToArgs(v),
							Env: withCredentialsEnv(v, []v1.EnvVar{
								{
									Name:  api.EnvVaultAddress,
									Value: fmt.Sprintf("https://%s.%s:8200", v.Name, v.Namespace),
								}, {
									Name:  api.EnvVaultCACert,
									Value: "/vault/tls/ca.crt",
								},
							}),
							VolumeMounts: withCredentialsVolumeMount(v, []v1.VolumeMount{{
								Name:      "config",
								MountPath: "/config",
							}, {
								Name:      "vault-tls",
								MountPath: "/vault/tls",
							}}),
							WorkingDir: "/config",
						},
					},
					Volumes: withCredentialsVolume(v, []v1.Volume{
						{
							Name: "config",
							VolumeSource: v1.VolumeSource{
								ConfigMap: &v1.ConfigMapVolumeSource{
									LocalObjectReference: v1.LocalObjectReference{Name: v.Name + "-configurer"},
								},
							},
						},
						{
							Name: "vault-tls",
							VolumeSource: v1.VolumeSource{
								Secret: &v1.SecretVolumeSource{
									SecretName: v.Name + "-tls",
								},
							},
						},
					}),
				},
			},
		},
	}
	addOwnerRefToObject(dep, asOwner(v))
	return dep
}

func configMapForConfigurer(v *v1alpha1.Vault) *v1.ConfigMap {
	ls := labelsForVaultConfigurer(v.Name)
	cm := &v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      v.Name + "-configurer",
			Namespace: v.Namespace,
			Labels:    ls,
		},
		Data: map[string]string{vault.DefaultConfigFile: v.Spec.ExternalConfigJSON()},
	}
	addOwnerRefToObject(cm, asOwner(v))
	return cm
}

// labelsForVault returns the labels for selecting the resources
// belonging to the given vault CR name.
func labelsForVault(name string) map[string]string {
	return map[string]string{"app": "vault", "vault_cr": name}
}

// labelsForVaultConfigurer returns the labels for selecting the resources
// belonging to the given vault CR name.
func labelsForVaultConfigurer(name string) map[string]string {
	return map[string]string{"app": "vault-configurator", "vault_cr": name}
}

// addOwnerRefToObject appends the desired OwnerReference to the object
func addOwnerRefToObject(obj metav1.Object, ownerRef metav1.OwnerReference) {
	obj.SetOwnerReferences(append(obj.GetOwnerReferences(), ownerRef))
}

// asOwner returns an OwnerReference set as the vault CR
func asOwner(v *v1alpha1.Vault) metav1.OwnerReference {
	trueVar := true
	return metav1.OwnerReference{
		APIVersion: v.APIVersion,
		Kind:       v.Kind,
		Name:       v.Name,
		UID:        v.UID,
		Controller: &trueVar,
	}
}

// podList returns a v1.PodList object
func podList() *v1.PodList {
	return &v1.PodList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
	}
}

// getPodNames returns the pod names of the array of pods passed in
func getPodNames(pods []v1.Pod) []string {
	var podNames []string
	for _, pod := range pods {
		podNames = append(podNames, pod.Name)
	}
	return podNames
}
