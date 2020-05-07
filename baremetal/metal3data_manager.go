/*
Copyright 2020 The Kubernetes Authors.

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

package baremetal

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	bmo "github.com/metal3-io/baremetal-operator/pkg/apis/metal3/v1alpha1"
	capm3 "github.com/metal3-io/cluster-api-provider-metal3/api/v1alpha4"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	capi "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// DataManagerInterface is an interface for a DataManager
type DataManagerInterface interface {
	SetFinalizer()
	UnsetFinalizer()
	Reconcile(ctx context.Context) error
	ReleaseLeases(ctx context.Context) error
}

// DataManager is responsible for performing machine reconciliation
type DataManager struct {
	client client.Client
	Data   *capm3.Metal3Data
	Log    logr.Logger
}

// NewDataManager returns a new helper for managing a Metal3Data object
func NewDataManager(client client.Client,
	data *capm3.Metal3Data, dataLog logr.Logger) (*DataManager, error) {

	return &DataManager{
		client: client,
		Data:   data,
		Log:    dataLog,
	}, nil
}

// SetFinalizer sets finalizer
func (m *DataManager) SetFinalizer() {
	// If the Metal3Data doesn't have finalizer, add it.
	if !Contains(m.Data.Finalizers, capm3.DataFinalizer) {
		m.Data.Finalizers = append(m.Data.Finalizers,
			capm3.DataFinalizer,
		)
	}
}

// UnsetFinalizer unsets finalizer
func (m *DataManager) UnsetFinalizer() {
	// Remove the finalizer.
	m.Data.Finalizers = Filter(m.Data.Finalizers,
		capm3.DataFinalizer,
	)
}

func (m *DataManager) clearError(ctx context.Context) {
	m.Data.Status.Error = false
	m.Data.Status.ErrorMessage = nil
}

func (m *DataManager) setError(ctx context.Context, msg string) {
	m.Data.Status.Error = true
	m.Data.Status.ErrorMessage = &msg
}

func (m *DataManager) Reconcile(ctx context.Context) error {
	m.clearError(ctx)

	if err := m.createSecrets(ctx); err != nil {
		if _, ok := errors.Cause(err).(HasRequeueAfterError); ok {
			return err
		}
		m.setError(ctx, errors.Cause(err).Error())
		return err
	}

	return nil
}

// CreateSecrets creates the secret if they do not exist.
func (m *DataManager) createSecrets(ctx context.Context) error {
	var metaDataErr, networkDataErr error

	if m.Data.Spec.DataTemplate == nil {
		return nil
	}
	if m.Data.Spec.DataTemplate.Namespace == "" {
		m.Data.Spec.DataTemplate.Namespace = m.Data.Namespace
	}
	// Fetch the Metal3DataTemplate object to get the templates
	m3dt, err := fetchM3DataTemplate(ctx, m.Data.Spec.DataTemplate, m.client,
		m.Log, m.Data.Labels[capi.ClusterLabelName],
	)
	if err != nil {
		return err
	}
	if m3dt == nil {
		return nil
	}
	m.Log.Info("Fetched Metal3DataTemplate")

	// Fetch the Metal3Machine, to get the related info
	m3m, err := m.getM3Machine(ctx, m3dt)
	if err != nil {
		return err
	}
	if m3m == nil {
		return errors.New("Unexpected error getching Metal3Machine")
	}
	m.Log.Info("Fetched Metal3Machine")

	// If the MetaData is given as part of Metal3DataTemplate
	if m3dt.Spec.MetaData != nil {
		// If the secret name is unset, set it
		if m.Data.Spec.MetaData == nil || m.Data.Spec.MetaData.Name == "" {
			m.Data.Spec.MetaData = &corev1.SecretReference{
				Name:      m3m.Name + "-metadata",
				Namespace: m.Data.Namespace,
			}
		}

		// Try to fetch the secret. If it exists, we do not modify it, to be able
		// to reprovision a node in the exact same state.
		m.Log.Info("Checking if secret exists", "secret", m.Data.Spec.MetaData.Name)
		_, metaDataErr = checkSecretExists(m.client, ctx, m.Data.Spec.MetaData.Name,
			m.Data.Namespace,
		)

		if metaDataErr != nil && !apierrors.IsNotFound(metaDataErr) {
			return metaDataErr
		}
		if apierrors.IsNotFound(metaDataErr) {
			m.Log.Info("MetaData secret creation needed", "secret", m.Data.Spec.MetaData.Name)
		}
	}

	// If the NetworkData is given as part of Metal3DataTemplate
	if m3dt.Spec.NetworkData != nil {
		// If the secret name is unset, set it
		if m.Data.Spec.NetworkData == nil || m.Data.Spec.NetworkData.Name == "" {
			m.Data.Spec.NetworkData = &corev1.SecretReference{
				Name:      m3m.Name + "-networkdata",
				Namespace: m.Data.Namespace,
			}
		}

		// Try to fetch the secret. If it exists, we do not modify it, to be able
		// to reprovision a node in the exact same state.
		m.Log.Info("Checking if secret exists", "secret", m.Data.Spec.NetworkData.Name)
		_, networkDataErr = checkSecretExists(m.client, ctx, m.Data.Spec.NetworkData.Name,
			m.Data.Namespace,
		)
		if networkDataErr != nil && !apierrors.IsNotFound(networkDataErr) {
			return networkDataErr
		}
		if apierrors.IsNotFound(networkDataErr) {
			m.Log.Info("NetworkData secret creation needed", "secret", m.Data.Spec.NetworkData.Name)
		}
	}

	// No secret needs creation
	if metaDataErr == nil && networkDataErr == nil {
		m.Log.Info("Metal3Data Reconciled")
		m.Data.Status.Ready = true
		return nil
	}

	// Fetch all the Metal3IPPools and set the OwnerReference. Check if the
	// IP address has been allocated, if so, fetch the address, gateway and prefix.
	poolAddresses, err := m.getAddressesFromPool(ctx, *m3dt)
	if err != nil {
		return err
	}

	// Fetch the Machine.
	capiMachine, err := util.GetOwnerMachine(ctx, m.client, m3m.ObjectMeta)

	if err != nil {
		return errors.Wrapf(err, "Metal3Machine's owner Machine could not be retrieved")
	}
	if capiMachine == nil {
		m.Log.Info("Waiting for Machine Controller to set OwnerRef on Metal3Machine")
		return &RequeueAfterError{RequeueAfter: requeueAfter}
	}
	m.Log.Info("Fetched Machine")

	// Fetch the BMH associated with the M3M
	bmh, err := getHost(ctx, m3m, m.client, m.Log)
	if err != nil {
		return err
	}
	if bmh == nil {
		return &RequeueAfterError{RequeueAfter: requeueAfter}
	}
	m.Log.Info("Fetched BMH")

	// Create the owner Ref for the secret
	ownerRefs := []metav1.OwnerReference{
		metav1.OwnerReference{
			Controller: pointer.BoolPtr(true),
			APIVersion: m.Data.APIVersion,
			Kind:       m.Data.Kind,
			Name:       m.Data.Name,
			UID:        m.Data.UID,
		},
	}

	// The MetaData secret must be created
	if apierrors.IsNotFound(metaDataErr) {
		m.Log.Info("Creating Metadata secret")
		metadata, err := renderMetaData(m.Data, m3dt, m3m, capiMachine, bmh, poolAddresses)
		if err != nil {
			return err
		}
		if err := createSecret(m.client, ctx, m.Data.Spec.MetaData.Name,
			m.Data.Namespace, m3dt.Labels[capi.ClusterLabelName],
			ownerRefs, map[string][]byte{"metaData": metadata},
		); err != nil {
			return err
		}
	}

	// The NetworkData secret must be created
	if apierrors.IsNotFound(networkDataErr) {
		m.Log.Info("Creating Networkdata secret")
		networkData, err := renderNetworkData(m.Data, m3dt, bmh, poolAddresses)
		if err != nil {
			return err
		}
		if err := createSecret(m.client, ctx, m.Data.Spec.NetworkData.Name,
			m.Data.Namespace, m3dt.Labels[capi.ClusterLabelName],
			ownerRefs, map[string][]byte{"networkData": networkData},
		); err != nil {
			return err
		}
	}

	m.Log.Info("Metal3Data reconciled")
	m.Data.Status.Ready = true
	return nil
}

// CreateSecrets creates the secret if they do not exist.
func (m *DataManager) ReleaseLeases(ctx context.Context) error {
	if m.Data.Spec.DataTemplate == nil {
		return nil
	}
	if m.Data.Spec.DataTemplate.Namespace == "" {
		m.Data.Spec.DataTemplate.Namespace = m.Data.Namespace
	}
	// Fetch the Metal3DataTemplate object to get the templates
	m3dt, err := fetchM3DataTemplate(ctx, m.Data.Spec.DataTemplate, m.client,
		m.Log, m.Data.Labels[capi.ClusterLabelName],
	)
	if err != nil {
		return err
	}
	if m3dt == nil {
		return nil
	}
	m.Log.Info("Fetched Metal3DataTemplate")

	return m.releaseAddressesFromPool(ctx, *m3dt)
}

// addressFromPool contains the address, prefix and gateway coming from a pool
type addressFromPool struct {
	address capm3.IPAddress
	prefix  int
	gateway capm3.IPAddress
}

// getAddressesFromPool will fetch each Metal3IPPool referenced at least once,
// set the Ownerreference if not set, and check if the Metal3IPAddress has been
// allocated. If not, it will requeue once all pools were fetched. If all have
// been allocated it will return a map containing the pool name and the address,
// prefix and gateway from that pool
func (m *DataManager) getAddressesFromPool(ctx context.Context,
	m3dt capm3.Metal3DataTemplate,
) (map[string]addressFromPool, error) {
	var err error
	requeue := false
	itemRequeue := false
	addresses := make(map[string]addressFromPool)
	if m3dt.Spec.MetaData != nil {
		for _, pool := range m3dt.Spec.MetaData.IPAddressesFromPool {
			addresses, itemRequeue, err = m.getAddressFromPool(ctx, pool.Name, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return addresses, err
			}
		}
		for _, pool := range m3dt.Spec.MetaData.PrefixesFromPool {
			addresses, itemRequeue, err = m.getAddressFromPool(ctx, pool.Name, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return addresses, err
			}
		}
		for _, pool := range m3dt.Spec.MetaData.GatewaysFromPool {
			addresses, itemRequeue, err = m.getAddressFromPool(ctx, pool.Name, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return addresses, err
			}
		}
	}
	if m3dt.Spec.NetworkData != nil {
		for _, network := range m3dt.Spec.NetworkData.Networks.IPv4 {
			addresses, itemRequeue, err = m.getAddressFromPool(ctx, network.IPAddressFromIPPool, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return addresses, err
			}
			// network.IPAddressFromIPPool
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.getAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return addresses, err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv6 {
			addresses, itemRequeue, err = m.getAddressFromPool(ctx, network.IPAddressFromIPPool, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return addresses, err
			}
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.getAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return addresses, err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv4DHCP {
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.getAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return addresses, err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv6DHCP {
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.getAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return addresses, err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv6SLAAC {
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.getAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return addresses, err
					}
				}
			}
		}
	}
	if requeue {
		return addresses, &RequeueAfterError{RequeueAfter: requeueAfter}
	}
	return addresses, nil
}

// releaseAddressesFromPool removes the OwnerReference on the IPPool objects
func (m *DataManager) releaseAddressesFromPool(ctx context.Context,
	m3dt capm3.Metal3DataTemplate,
) error {
	var err error
	requeue := false
	itemRequeue := false
	addresses := make(map[string]bool)
	if m3dt.Spec.MetaData != nil {
		for _, pool := range m3dt.Spec.MetaData.IPAddressesFromPool {
			addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, pool.Name, addresses)
			requeue = requeue || itemRequeue
			fmt.Println(requeue)
			if err != nil {
				return err
			}
		}
		for _, pool := range m3dt.Spec.MetaData.PrefixesFromPool {
			addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, pool.Name, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return err
			}
		}
		for _, pool := range m3dt.Spec.MetaData.GatewaysFromPool {
			addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, pool.Name, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return err
			}
		}
	}
	if m3dt.Spec.NetworkData != nil {
		for _, network := range m3dt.Spec.NetworkData.Networks.IPv4 {
			addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, network.IPAddressFromIPPool, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return err
			}
			// network.IPAddressFromIPPool
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv6 {
			addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, network.IPAddressFromIPPool, addresses)
			requeue = requeue || itemRequeue
			if err != nil {
				return err
			}
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv4DHCP {
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv6DHCP {
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return err
					}
				}
			}
		}

		for _, network := range m3dt.Spec.NetworkData.Networks.IPv6SLAAC {
			for _, route := range network.Routes {
				if route.Gateway.FromIPPool != nil {
					addresses, itemRequeue, err = m.releaseAddressFromPool(ctx, *route.Gateway.FromIPPool, addresses)
					requeue = requeue || itemRequeue
					if err != nil {
						return err
					}
				}
			}
		}
	}
	if requeue {
		return &RequeueAfterError{RequeueAfter: requeueAfter}
	}
	return nil
}

// getAddressFromPool adds an ownerReference on the referenced Metal3IPPool
// objects. It then tries to fetch the Metal3IPAddress if it exists, and asks
// for requeue if not ready.
func (m *DataManager) getAddressFromPool(ctx context.Context, poolName string,
	addresses map[string]addressFromPool,
) (map[string]addressFromPool, bool, error) {

	if addresses == nil {
		addresses = make(map[string]addressFromPool)
	}
	if entry, ok := addresses[poolName]; ok {
		if entry.address != "" {
			return addresses, false, nil
		}
	}
	addresses[poolName] = addressFromPool{}
	// Get the IPPool object
	// Fetch the Metal3IPPool instance.
	capm3IPPool := &capm3.Metal3IPPool{}
	poolNamespacedName := types.NamespacedName{
		Name:      poolName,
		Namespace: m.Data.Namespace,
	}

	if err := m.client.Get(ctx, poolNamespacedName, capm3IPPool); err != nil {
		if apierrors.IsNotFound(err) {
			return addresses, true, nil
		}
		return addresses, false, err
	}

	// Verify that the owner reference is there, if not add it and update object,
	// if error requeue.
	_, err := findOwnerRefFromList(capm3IPPool.OwnerReferences,
		m.Data.TypeMeta, m.Data.ObjectMeta)
	if err != nil {
		if _, ok := err.(*NotFoundError); !ok {
			return addresses, false, err
		}
		capm3IPPool.OwnerReferences, err = setOwnerRefInList(
			capm3IPPool.OwnerReferences, false, m.Data.TypeMeta, m.Data.ObjectMeta,
		)
		if err != nil {
			return addresses, false, err
		}
		err = updateObject(m.client, ctx, capm3IPPool)
		if err != nil {
			if _, ok := err.(*RequeueAfterError); !ok {
				return addresses, false, err
			}
			return addresses, true, nil
		}
	}

	// verify if allocation is there, if not requeue
	addressName, ok := capm3IPPool.Status.Allocations[m.Data.Name]
	if !ok {
		return addresses, true, nil
	}
	if addressName == "" {
		errorStr := fmt.Sprintf(
			"%s Unable to allocate IP address, pool exhausted ?", poolName,
		)
		m.setError(ctx, errorStr)
		return addresses, false, errors.New(errorStr)
	}

	// get Metal3IPAddress object
	capm3IPAddress := &capm3.Metal3IPAddress{}
	addressNamespacedName := types.NamespacedName{
		Name:      addressName,
		Namespace: m.Data.Namespace,
	}

	if err := m.client.Get(ctx, addressNamespacedName, capm3IPAddress); err != nil {
		if apierrors.IsNotFound(err) {
			return addresses, true, nil
		}
		return addresses, false, err
	}

	// get address, gateway and prefixes
	addresses[poolName] = addressFromPool{
		address: capm3IPAddress.Spec.Address,
		prefix:  capm3IPAddress.Spec.Prefix,
		gateway: *capm3IPAddress.Spec.Gateway,
	}

	return addresses, false, nil
}

// releaseAddressFromPool removes the owner reference on existing referenced
// Metal3IPPool objects
func (m *DataManager) releaseAddressFromPool(ctx context.Context, poolName string,
	addresses map[string]bool,
) (map[string]bool, bool, error) {

	if addresses == nil {
		addresses = make(map[string]bool)
	}
	if succeeded, ok := addresses[poolName]; ok {
		if succeeded {
			return addresses, false, nil
		}
	}
	addresses[poolName] = false
	// Get the IPPool object
	// Fetch the Metal3IPPool instance.
	capm3IPPool := &capm3.Metal3IPPool{}
	poolNamespacedName := types.NamespacedName{
		Name:      poolName,
		Namespace: m.Data.Namespace,
	}

	if err := m.client.Get(ctx, poolNamespacedName, capm3IPPool); err != nil {
		if apierrors.IsNotFound(err) {
			addresses[poolName] = true
			return addresses, false, nil
		}
		return addresses, false, err
	}

	// Verify that the owner reference is there, if not add it and update object,
	// if error requeue.
	_, err := findOwnerRefFromList(capm3IPPool.OwnerReferences,
		m.Data.TypeMeta, m.Data.ObjectMeta)
	if err != nil {
		if _, ok := err.(*NotFoundError); !ok {
			return addresses, false, err
		}
	} else {
		capm3IPPool.OwnerReferences, err = deleteOwnerRefFromList(
			capm3IPPool.OwnerReferences, m.Data.TypeMeta, m.Data.ObjectMeta,
		)
		if err != nil {
			return addresses, false, err
		}
		err = updateObject(m.client, ctx, capm3IPPool)
		if err != nil {
			if _, ok := err.(*RequeueAfterError); !ok {
				return addresses, false, err
			}
			return addresses, true, nil
		}
	}
	addresses[poolName] = true
	return addresses, false, nil
}

// renderNetworkData renders the networkData into an object that will be
// marshalled into the secret
func renderNetworkData(m3d *capm3.Metal3Data, m3dt *capm3.Metal3DataTemplate,
	bmh *bmo.BareMetalHost, poolAddresses map[string]addressFromPool,
) ([]byte, error) {
	if m3dt.Spec.NetworkData == nil {
		return nil, nil
	}
	var err error

	networkData := map[string][]interface{}{}

	networkData["links"], err = renderNetworkLinks(m3dt.Spec.NetworkData.Links, bmh)
	if err != nil {
		return nil, err
	}

	networkData["networks"], err = renderNetworkNetworks(
		m3dt.Spec.NetworkData.Networks, m3d, poolAddresses,
	)
	if err != nil {
		return nil, err
	}

	networkData["services"] = renderNetworkServices(m3dt.Spec.NetworkData.Services)

	return yaml.Marshal(networkData)
}

// renderNetworkServices renders the services
func renderNetworkServices(services capm3.NetworkDataService) []interface{} {
	data := []interface{}{}

	for _, service := range services.DNS {
		data = append(data, map[string]interface{}{
			"type":    "dns",
			"address": service,
		})
	}

	return data
}

// renderNetworkLinks renders the different types of links
func renderNetworkLinks(networkLinks capm3.NetworkDataLink, bmh *bmo.BareMetalHost) ([]interface{}, error) {
	data := []interface{}{}

	// Ethernet links
	for _, link := range networkLinks.Ethernets {
		mac_address, err := getLinkMacAddress(link.MACAddress, bmh)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":                 link.Type,
			"id":                   link.Id,
			"mtu":                  link.MTU,
			"ethernet_mac_address": mac_address,
		})
	}

	// Bond links
	for _, link := range networkLinks.Bonds {
		mac_address, err := getLinkMacAddress(link.MACAddress, bmh)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":                 "bond",
			"id":                   link.Id,
			"mtu":                  link.MTU,
			"ethernet_mac_address": mac_address,
			"bond_mode":            link.BondMode,
			"bond_links":           link.BondLinks,
		})
	}

	// Vlan links
	for _, link := range networkLinks.Vlans {
		mac_address, err := getLinkMacAddress(link.MACAddress, bmh)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":             "vlan",
			"id":               link.Id,
			"mtu":              link.MTU,
			"vlan_mac_address": mac_address,
			"vlan_id":          link.VlanID,
			"vlan_link":        link.VlanLink,
		})
	}

	return data, nil
}

// renderNetworkNetworks renders the different types of network
func renderNetworkNetworks(networks capm3.NetworkDataNetwork,
	m3d *capm3.Metal3Data, poolAddresses map[string]addressFromPool,
) ([]interface{}, error) {
	data := []interface{}{}

	// IPv4 networks static allocation
	for _, network := range networks.IPv4 {
		poolAddress, ok := poolAddresses[network.IPAddressFromIPPool]
		if !ok {
			return nil, errors.New("Pool not found in cache")
		}
		ip := capm3.IPAddressv4(poolAddress.address)
		mask := translateMask(poolAddress.prefix, true)
		routes, err := getRoutesv4(network.Routes, poolAddresses)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":       "ipv4",
			"id":         network.ID,
			"link":       network.Link,
			"netmask":    mask,
			"ip_address": ip,
			"routes":     routes,
		})
	}

	// IPv6 networks static allocation
	for _, network := range networks.IPv6 {
		poolAddress, ok := poolAddresses[network.IPAddressFromIPPool]
		if !ok {
			return nil, errors.New("Pool not found in cache")
		}
		ip := capm3.IPAddressv6(poolAddress.address)
		mask := translateMask(poolAddress.prefix, false)
		routes, err := getRoutesv6(network.Routes, poolAddresses)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":       "ipv6",
			"id":         network.ID,
			"link":       network.Link,
			"netmask":    mask,
			"ip_address": ip,
			"routes":     routes,
		})
	}

	// IPv4 networks DHCP allocation
	for _, network := range networks.IPv4DHCP {
		routes, err := getRoutesv4(network.Routes, poolAddresses)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":   "ipv4_dhcp",
			"id":     network.ID,
			"link":   network.Link,
			"routes": routes,
		})
	}

	// IPv6 networks DHCP allocation
	for _, network := range networks.IPv6DHCP {
		routes, err := getRoutesv6(network.Routes, poolAddresses)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":   "ipv6_dhcp",
			"id":     network.ID,
			"link":   network.Link,
			"routes": routes,
		})
	}

	// IPv6 networks SLAAC allocation
	for _, network := range networks.IPv6SLAAC {
		routes, err := getRoutesv6(network.Routes, poolAddresses)
		if err != nil {
			return nil, err
		}
		data = append(data, map[string]interface{}{
			"type":   "ipv6_slaac",
			"id":     network.ID,
			"link":   network.Link,
			"routes": routes,
		})
	}

	return data, nil
}

// getRoutesv4 returns the IPv4 routes
func getRoutesv4(netRoutes []capm3.NetworkDataRoutev4,
	poolAddresses map[string]addressFromPool,
) ([]interface{}, error) {
	routes := []interface{}{}
	for _, route := range netRoutes {
		gateway := capm3.IPAddressv4("")
		if route.Gateway.String != nil {
			gateway = *route.Gateway.String
		} else if route.Gateway.FromIPPool != nil {
			poolAddress, ok := poolAddresses[*route.Gateway.FromIPPool]
			if !ok {
				return []interface{}{}, errors.New("Failed to fetch pool from cache")
			}
			gateway = capm3.IPAddressv4(poolAddress.gateway)
		}
		services := []interface{}{}
		for _, service := range route.Services.DNS {
			services = append(services, map[string]interface{}{
				"type":    "dns",
				"address": service,
			})
		}
		mask := translateMask(route.Prefix, true)
		routes = append(routes, map[string]interface{}{
			"network":  route.Network,
			"netmask":  mask,
			"gateway":  gateway,
			"services": services,
		})
	}
	return routes, nil
}

// getRoutesv6 returns the IPv6 routes
func getRoutesv6(netRoutes []capm3.NetworkDataRoutev6,
	poolAddresses map[string]addressFromPool,
) ([]interface{}, error) {
	routes := []interface{}{}
	for _, route := range netRoutes {
		gateway := capm3.IPAddressv6("")
		if route.Gateway.String != nil {
			gateway = *route.Gateway.String
		} else if route.Gateway.FromIPPool != nil {
			poolAddress, ok := poolAddresses[*route.Gateway.FromIPPool]
			if !ok {
				return []interface{}{}, errors.New("Failed to fetch pool from cache")
			}
			gateway = capm3.IPAddressv6(poolAddress.gateway)
		}
		services := []interface{}{}
		for _, service := range route.Services.DNS {
			services = append(services, map[string]interface{}{
				"type":    "dns",
				"address": service,
			})
		}
		mask := translateMask(route.Prefix, false)
		routes = append(routes, map[string]interface{}{
			"network":  route.Network,
			"netmask":  mask,
			"gateway":  gateway,
			"services": services,
		})
	}
	return routes, nil
}

// translateMask transforms a mask given as integer into a dotted-notation string
func translateMask(maskInt int, ipv4 bool) interface{} {
	if ipv4 {
		// Get the mask by concatenating the IPv4 prefix of net package and the mask
		address := net.IP(append([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255},
			[]byte(net.CIDRMask(maskInt, 32))...,
		)).String()
		return capm3.IPAddressv4(address)
	} else {
		// get the mask
		address := net.IP(net.CIDRMask(maskInt, 128)).String()
		return capm3.IPAddressv6(address)
	}
}

// getLinkMacAddress returns the mac address
func getLinkMacAddress(mac *capm3.NetworkLinkEthernetMac, bmh *bmo.BareMetalHost) (
	string, error,
) {
	mac_address := ""
	var err error

	// if a string was given
	if mac.String != nil {
		mac_address = *mac.String

		// Otherwise fetch the mac from the interface name
	} else if mac.FromHostInterface != nil {
		mac_address, err = getBMHMacByName(*mac.FromHostInterface, bmh)
	}

	return mac_address, err
}

// renderMetaData renders the MetaData items
func renderMetaData(m3d *capm3.Metal3Data, m3dt *capm3.Metal3DataTemplate,
	m3m *capm3.Metal3Machine, machine *capi.Machine, bmh *bmo.BareMetalHost,
	poolAddresses map[string]addressFromPool,
) ([]byte, error) {
	if m3dt.Spec.MetaData == nil {
		return nil, nil
	}
	metadata := make(map[string]string)

	// Mac addresses
	for _, entry := range m3dt.Spec.MetaData.FromHostInterfaces {
		value, err := getBMHMacByName(entry.Interface, bmh)
		if err != nil {
			return nil, err
		}
		metadata[entry.Key] = value
	}

	// IP addresses
	for _, entry := range m3dt.Spec.MetaData.IPAddressesFromPool {
		poolAddress, ok := poolAddresses[entry.Name]
		if !ok {
			return nil, errors.New("Pool not found in cache")
		}
		metadata[entry.Key] = string(poolAddress.address)
	}

	// Prefixes
	for _, entry := range m3dt.Spec.MetaData.PrefixesFromPool {
		poolAddress, ok := poolAddresses[entry.Name]
		if !ok {
			return nil, errors.New("Pool not found in cache")
		}
		metadata[entry.Key] = strconv.Itoa(poolAddress.prefix)
	}

	// Gateways
	for _, entry := range m3dt.Spec.MetaData.GatewaysFromPool {
		poolAddress, ok := poolAddresses[entry.Name]
		if !ok {
			return nil, errors.New("Pool not found in cache")
		}
		metadata[entry.Key] = string(poolAddress.gateway)
	}

	// Indexes
	for _, entry := range m3dt.Spec.MetaData.Indexes {
		if entry.Step == 0 {
			entry.Step = 1
		}
		metadata[entry.Key] = entry.Prefix + strconv.Itoa(entry.Offset+m3d.Spec.Index*entry.Step) + entry.Suffix
	}

	// Namespaces
	for _, entry := range m3dt.Spec.MetaData.Namespaces {
		metadata[entry.Key] = m3d.Namespace
	}

	// Object names
	for _, entry := range m3dt.Spec.MetaData.ObjectNames {
		switch strings.ToLower(entry.Object) {
		case "metal3machine":
			metadata[entry.Key] = m3m.Name
		case "machine":
			metadata[entry.Key] = machine.Name
		case "baremetalhost":
			metadata[entry.Key] = bmh.Name
		default:
			return nil, errors.New("Unknown object type")
		}
	}

	// Labels
	for _, entry := range m3dt.Spec.MetaData.FromLabels {
		switch strings.ToLower(entry.Object) {
		case "metal3machine":
			metadata[entry.Key] = m3m.Labels[entry.Label]
		case "machine":
			metadata[entry.Key] = machine.Labels[entry.Label]
		case "baremetalhost":
			metadata[entry.Key] = bmh.Labels[entry.Label]
		default:
			return nil, errors.New("Unknown object type")
		}
	}

	// Annotations
	for _, entry := range m3dt.Spec.MetaData.FromAnnotations {
		switch strings.ToLower(entry.Object) {
		case "metal3machine":
			metadata[entry.Key] = m3m.Annotations[entry.Annotation]
		case "machine":
			metadata[entry.Key] = machine.Annotations[entry.Annotation]
		case "baremetalhost":
			metadata[entry.Key] = bmh.Annotations[entry.Annotation]
		default:
			return nil, errors.New("Unknown object type")
		}
	}

	// Strings
	for _, entry := range m3dt.Spec.MetaData.Strings {
		metadata[entry.Key] = entry.Value
	}

	return yaml.Marshal(metadata)
}

// getIPAddress renders the IP address, taking the index, offset and step into
// account, it is IP version agnostic
func getIPAddress(entry capm3.IPPool, index int) (string, error) {

	if entry.Start == nil && entry.Subnet == nil {
		return "", errors.New("Either Start or Subnet is required for ipAddress")
	}
	var ip net.IP
	var err error
	var ipNet *net.IPNet
	offset := index

	// If start is given, use it to add the offset
	if entry.Start != nil {
		var endIP net.IP
		if entry.End != nil {
			endIP = net.ParseIP(string(*entry.End))
		}
		ip, err = addOffsetToIP(net.ParseIP(string(*entry.Start)), endIP, offset)
		if err != nil {
			return "", err
		}

		// Verify that the IP is in the subnet
		if entry.Subnet != nil {
			_, ipNet, err = net.ParseCIDR(string(*entry.Subnet))
			if err != nil {
				return "", err
			}
			if !ipNet.Contains(ip) {
				return "", errors.New("IP address out of bonds")
			}
		}

		// If it is not given, use the CIDR ip address and increment the offset by 1
	} else {
		ip, ipNet, err = net.ParseCIDR(string(*entry.Subnet))
		if err != nil {
			return "", err
		}
		offset++
		ip, err = addOffsetToIP(ip, nil, offset)
		if err != nil {
			return "", err
		}

		// Verify that the ip is in the subnet
		if !ipNet.Contains(ip) {
			return "", errors.New("IP address out of bonds")
		}
	}
	return ip.String(), nil
}

// addOffsetToIP computes the value of the IP address with the offset. It is
// IP version agnostic
// Note that if the resulting IP address is in the format ::ffff:xxxx:xxxx then
// ip.String will fail to select the correct type of ip
func addOffsetToIP(ip, endIP net.IP, offset int) (net.IP, error) {
	ip4 := false
	//ip := net.ParseIP(ipString)
	if ip.To4() != nil {
		ip4 = true
	}

	// Create big integers
	IPInt := big.NewInt(0)
	OffsetInt := big.NewInt(int64(offset))

	// Transform the ip into an int. (big endian function)
	IPInt = IPInt.SetBytes(ip)

	// add the two integers
	IPInt = IPInt.Add(IPInt, OffsetInt)

	// return the bytes list
	IPBytes := IPInt.Bytes()

	IPBytesLen := len(IPBytes)

	// Verify that the IPv4 or IPv6 fulfills theirs constraints
	if (ip4 && IPBytesLen > 6 && IPBytes[4] != 255 && IPBytes[5] != 255) ||
		(!ip4 && IPBytesLen > 16) {
		return nil, errors.New(fmt.Sprintf("IP address overflow for : %s", ip.String()))
	}

	//transform the end ip into an Int to compare
	if endIP != nil {
		endIPInt := big.NewInt(0)
		endIPInt = endIPInt.SetBytes(endIP)
		// Computed IP is higher than the end IP
		if IPInt.Cmp(endIPInt) > 0 {
			return nil, errors.New(fmt.Sprintf("IP address out of bonds for : %s", ip.String()))
		}
	}

	// COpy the output back into an ip
	copy(ip[16-IPBytesLen:], IPBytes)
	return ip, nil
}

// getBMHMacByName returns the mac address of the interface matching the name
func getBMHMacByName(name string, bmh *bmo.BareMetalHost) (string, error) {
	if bmh == nil || bmh.Status.HardwareDetails == nil || bmh.Status.HardwareDetails.NIC == nil {
		return "", errors.New("Nics list not populated")
	}
	for _, nics := range bmh.Status.HardwareDetails.NIC {
		if nics.Name == name {
			return nics.MAC, nil
		}
	}
	return "", errors.New(fmt.Sprintf("Nic name not found %v", name))
}

func (m *DataManager) getM3Machine(ctx context.Context, m3dt *capm3.Metal3DataTemplate) (*capm3.Metal3Machine, error) {
	if m.Data.Spec.Metal3Machine == nil {
		return nil, nil
	}
	if m.Data.Spec.Metal3Machine.Name == "" {
		return nil, errors.New("Metal3Machine name not set")
	}

	return getM3Machine(ctx, m.client, m.Log,
		m.Data.Spec.Metal3Machine.Name, m.Data.Namespace, m3dt, true,
	)
}
