// Copyright 2019, Verizon Media Inc.
// Licensed under the terms of the 3-Clause BSD license. See LICENSE file in
// github.com/yahoo/k8s-athenz-istio-auth for terms.
package common

import (
	"fmt"
	"github.com/ghodss/yaml"
	"github.com/gogo/protobuf/proto"
	"github.com/yahoo/athenz/clients/go/zms"
	"github.com/yahoo/k8s-athenz-istio-auth/pkg/log"
	"io/ioutil"
	"istio.io/istio/pilot/pkg/config/kube/crd"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/config/schema/collections"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	allUsers                     = "user.*"
	WildCardAll                  = "*"
	ServiceRoleKind              = "ServiceRole"
	AthenzJwtPrefix              = "athenz/"
	RequestAuthPrincipalProperty = "request.auth.principal"
	DryRunStoredFilesDirectory   = "/root/authzpolicy/"
)

var supportedMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodConnect: true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
	"*":                true,
}

var resourceRegex = regexp.MustCompile(`\A(?P<domain>.*):svc.(?P<svc>[^:]*)[:]?(?P<path>.*)\z`)

type Item struct {
	Operation model.Event
	Resource  model.Config
	// Handler function that should be invoked with the status of the current sync operation on the item
	// If the handler returns an error, the operation is retried up to `queueNumRetries`
	CallbackHandler OnCompleteFunc
}

type OnCompleteFunc func(err error, item *Item) error
type additionalCheck func(model.Config) bool

type EventHandler interface {
	Add(item *Item) error
	Update(item *Item) error
	Delete(item *Item) error
}

type DryRunHandler struct{}

func (d *DryRunHandler) Add(item *Item) error {
	return d.createDryrunResource(item, DryRunStoredFilesDirectory)
}

func (d *DryRunHandler) Update(item *Item) error {
	return d.createDryrunResource(item, DryRunStoredFilesDirectory)
}

func (d *DryRunHandler) Delete(item *Item) error {
	return d.findDeleteDryrunResource(item, DryRunStoredFilesDirectory)
}

type ApiHandler struct {
	ConfigStoreCache model.ConfigStoreCache
}

func (a *ApiHandler) Add(item *Item) error {
	_, err := a.ConfigStoreCache.Create(item.Resource)
	return err
}

func (a *ApiHandler) Update(item *Item) error {
	_, err := a.ConfigStoreCache.Update(item.Resource)
	return err
}

func (a *ApiHandler) Delete(item *Item) error {
	res := item.Resource
	err := a.ConfigStoreCache.Delete(collections.IstioSecurityV1Beta1Authorizationpolicies.Resource().GroupVersionKind(), res.Name, res.Namespace)
	return err
}

// MemberToSpiffe parses the Athenz role member into a SPIFFE compliant name.
// Example: example.domain/sa/service
func MemberToSpiffe(member *zms.RoleMember) (string, error) {

	if member == nil {
		return "", fmt.Errorf("member is nil")
	}

	memberStr := string(member.MemberName)

	// special condition: if member == 'user.*', return '*'
	if memberStr == allUsers {
		return WildCardAll, nil
	}

	return PrincipalToSpiffe(memberStr)
}

// MemberToOriginSubject parses the Athenz role member into the request.auth.principal
// jwt format. Example: athenz/example.domain.service
func MemberToOriginJwtSubject(member *zms.RoleMember) (string, error) {

	if member == nil {
		return "", fmt.Errorf("member is nil")
	}

	memberStr := string(member.MemberName)

	// special condition: if member == 'user.*', return '*'
	if memberStr == allUsers {
		return WildCardAll, nil
	}

	requestAuthPrincipal := AthenzJwtPrefix + memberStr
	return requestAuthPrincipal, nil
}

// RoleToSpiffe reads athenz role name string, and generates the SPIFFE name of it
// SPIFFE name format: <athenz domain name>/ra/<role name>
func RoleToSpiffe(athenzDomainName string, roleName string) (string, error) {
	if len(athenzDomainName) == 0 {
		return "", fmt.Errorf("athenzDomainName is empty")
	}
	if len(roleName) == 0 {
		return "", fmt.Errorf("roleName is empty")
	}
	spiffeName := fmt.Sprintf("%s/ra/%s", athenzDomainName, roleName)
	return spiffeName, nil
}

// PrincipalToSpiffe converts the Athenz principal into a SPIFFE compliant format
// e.g. client-domain.frontend.some-app -> client-domain.frontend/sa/some-app
func PrincipalToSpiffe(principal string) (string, error) {
	if len(principal) == 0 {
		return "", fmt.Errorf("principal is empty")
	}
	i := strings.LastIndex(principal, ".")
	if i < 0 {
		return "", fmt.Errorf("principal:%s is not of the format <Athenz-domain>.<Athenz-service>", principal)
	}
	memberDomain, memberService := principal[:i], principal[i+1:]
	spiffeName := fmt.Sprintf("%s/sa/%s", memberDomain, memberService)
	return spiffeName, nil
}

// ParseAssertionEffect parses the effect of an assertion into a supported Istio RBAC action
func ParseAssertionEffect(assertion *zms.Assertion) (string, error) {
	if assertion == nil {
		return "", fmt.Errorf("assertion is nil")
	}
	effect := assertion.Effect
	if effect == nil {
		return "", fmt.Errorf("assertion effect is nil")
	}
	if strings.ToUpper(effect.String()) != zms.ALLOW.String() {
		return "", fmt.Errorf("effect: %s is not a supported assertion effect", effect)
	}
	return zms.ALLOW.String(), nil
}

// ParseAssertionAction parses the action of an assertion into a supported Istio RBAC HTTP method
func ParseAssertionAction(assertion *zms.Assertion) (string, error) {
	if assertion == nil {
		return "", fmt.Errorf("assertion is nil")
	}
	method := strings.ToUpper(assertion.Action)
	if !supportedMethods[method] {
		return "", fmt.Errorf("method: %s is not a supported HTTP method", assertion.Action)
	}
	return method, nil
}

// ParseAssertionResource parses the resource of an action into the service name (AccessRule constraint) and the
// HTTP paths if specified (suffix :<path>)
func ParseAssertionResource(domainName zms.DomainName, assertion *zms.Assertion) (string, string, error) {
	if assertion == nil {
		return "", "", fmt.Errorf("assertion is nil")
	}
	var svc string
	var path string
	resource := assertion.Resource
	parts := resourceRegex.FindStringSubmatch(resource)
	names := resourceRegex.SubexpNames()
	results := map[string]string{}
	for i, part := range parts {
		results[names[i]] = part
	}

	for name, match := range results {
		switch name {
		case "domain":
			if match != string(domainName) {
				return "", "", fmt.Errorf("resource: %s does not belong to the Athenz domain: %s", resource, domainName)
			}
		case "svc":
			svc = match
		case "path":
			path = match
		}
	}

	if svc == "" {
		return "", "", fmt.Errorf("resource: %s does not specify the service using svc.<service-name> format", resource)
	}
	return svc, path, nil
}

// CheckAthenzSystemDisabled checks if athenz domain is systematically disabled, if so, controller skips processing current
// role member
func CheckAthenzSystemDisabled(roleMember *zms.RoleMember) (bool, error) {
	if roleMember == nil {
		return false, fmt.Errorf("got an empty role Member: %s, skipping", roleMember.MemberName)
	}
	if roleMember.SystemDisabled != nil && *roleMember.SystemDisabled != 0 {
		return false, fmt.Errorf("member %s is system disabled", roleMember.MemberName)
	}
	return true, nil
}

// CheckAthenzMemberExpiry checks if Expiration field (timezone UTC) is set in the roleMember object, and then
// checks if expiration date surpasses current time
func CheckAthenzMemberExpiry(roleMember *zms.RoleMember) (bool, error) {
	if roleMember == nil {
		return false, fmt.Errorf("got an empty role Member: %s, skipping", roleMember.MemberName)
	}
	// check if roleMember has expiration field set
	if roleMember.Expiration != nil && roleMember.Expiration.Before(time.Now()) {
		return false, fmt.Errorf("member %s is expired", roleMember.MemberName)
	}
	return true, nil
}

// ParseRoleFQDN parses the Athenz role full name in the format <domainName>:role.<roleName> to roleName
// e.g. app-domain:role.reader -> reader
func ParseRoleFQDN(domainName zms.DomainName, roleFQDN string) (string, error) {
	roleName := strings.TrimPrefix(roleFQDN, fmt.Sprintf("%s:role.", domainName))
	if strings.Contains(roleName, ":") {
		return "", fmt.Errorf("role: %s does not belong to the Athenz domain: %s", roleFQDN, domainName)
	}
	return roleName, nil
}

// NewConfig returns a new model.Config resource for the passed-in type with the given namespace/name and spec
func NewConfig(schema collection.Schema, namespace string, name string, spec proto.Message) model.Config {
	meta := model.ConfigMeta{
		Type:      schema.Resource().Kind(),
		Group:     schema.Resource().Group(),
		Version:   schema.Resource().Version(),
		Namespace: namespace,
		Name:      name,
	}
	return model.Config{
		ConfigMeta: meta,
		Spec:       spec,
	}
}

// ConvertSliceToKeyedMap converts the input model.Config slice into a map with (type/namespace/name) formatted key
func ConvertSliceToKeyedMap(in []model.Config) map[string]model.Config {
	out := make(map[string]model.Config, len(in))
	for _, c := range in {
		key := c.Key()
		out[key] = c
	}
	return out
}

// Equal compares the Spec of two model.Config items
func Equal(c1, c2 model.Config) bool {
	return c1.Key() == c2.Key() && proto.Equal(c1.Spec, c2.Spec)
}

// ComputeChangeList checks if two set of config models have any differences, and return its changeList
// Controller which calls this function is required to pass its own callback handler
// checkFn is optional, can be set to nil if nothing needs to be checked
func ComputeChangeList(currentCRs []model.Config, desiredCRs []model.Config, cbHandler OnCompleteFunc, checkFn additionalCheck) []*Item {
	currMap := ConvertSliceToKeyedMap(currentCRs)
	desiredMap := ConvertSliceToKeyedMap(desiredCRs)

	changeList := make([]*Item, 0)

	// loop through the desired slice of model.Config and add the items that need to be created or updated
	for _, desiredConfig := range desiredCRs {
		key := desiredConfig.Key()
		existingConfig, exists := currMap[key]
		// case 1: current CR is empty, desired CR is not empty, results in resource creation
		if !exists {
			item := Item{
				Operation:       model.EventAdd,
				Resource:        desiredConfig,
				CallbackHandler: cbHandler,
			}
			changeList = append(changeList, &item)
			continue
		}

		if !Equal(existingConfig, desiredConfig) {
			// case 2: current CR is not empty, desired CR is not empty, current CR != desired CR, and additional check is not set or not true,
			// results in resource update
			if checkFn != nil && checkFn(existingConfig) {
				continue
			}
			// copy metadata(for resource version) from current config to desired config
			desiredConfig.ConfigMeta = existingConfig.ConfigMeta
			item := Item{
				Operation:       model.EventUpdate,
				Resource:        desiredConfig,
				CallbackHandler: cbHandler,
			}
			changeList = append(changeList, &item)
			continue
		}
		// case 3: current CR is not empty, desired CR is not empty, current CR == desired CR, results in no action
	}

	// loop through the current slice of model.Config and add the items that needs to be deleted
	for _, currConfig := range currentCRs {
		key := currConfig.Key()
		_, exists := desiredMap[key]
		if checkFn != nil && checkFn(currConfig) {
			continue
		}
		// case 4: current CR is not empty, desired CR is empty, results in resource deletion
		if !exists {
			item := Item{
				Operation:       model.EventDelete,
				Resource:        currConfig,
				CallbackHandler: cbHandler,
			}
			changeList = append(changeList, &item)
		}
	}

	return changeList
}

// createDryrunResource creates the yaml file of given authorization policy spec and a local directory path
func (d *DryRunHandler) createDryrunResource(item *Item, localDirPath string) error {
	convertedCR := item.Resource
	authzPolicyName := item.Resource.ConfigMeta.Name
	namespace := item.Resource.ConfigMeta.Namespace
	convertedObj, err := crd.ConvertConfig(collections.IstioSecurityV1Beta1Authorizationpolicies, convertedCR)
	if err != nil {
		return fmt.Errorf("unable to convert authorization policy config to istio objects, resource name: %v", convertedCR.Name)
	}
	configInBytes, err := yaml.Marshal(convertedObj)
	if err != nil {
		return fmt.Errorf("could not marshal %v: %v", convertedCR.Name, err)
	}
	yamlFileName := authzPolicyName + "--" + namespace + ".yaml"
	return ioutil.WriteFile(localDirPath+yamlFileName, configInBytes, 0644)
}

// findDeleteDryrunResource retrieves the yaml file from local directory and deletes it
func (d *DryRunHandler) findDeleteDryrunResource(item *Item, localDirPath string) error {
	authzPolicyName := item.Resource.ConfigMeta.Name
	namespace := item.Resource.ConfigMeta.Namespace
	yamlFileName := authzPolicyName + "--" + namespace + ".yaml"
	if _, err := os.Stat(localDirPath + yamlFileName); os.IsNotExist(err) {
		log.Infof("file %s does not exist in local directory", localDirPath+yamlFileName)
		return nil
	} else if err != nil {
		return fmt.Errorf("error stating file %s in local directory, error: %s", localDirPath+yamlFileName, err)
	}
	log.Infof("deleting file under path: %s\n", localDirPath+yamlFileName)
	return os.Remove(localDirPath + yamlFileName)
}

// ReadConvertToModelConfig reads in the authorization policy yaml object and converts it into a model.Config struct
func ReadConvertToModelConfig(serviceName, namespace, localDirPath string) (*model.Config, error) {
	// define istio object interface to unmarshal yaml object into
	item := &crd.IstioKind{Spec: map[string]interface{}{}}
	yamlFileName := serviceName + "--" + namespace + ".yaml"
	yamlFile, err := ioutil.ReadFile(localDirPath + yamlFileName)
	if err != nil {
		return nil, fmt.Errorf("unable to read yaml file to local directory, err: %s", err)
	}
	err = yaml.Unmarshal(yamlFile, item)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal yaml file, err: %s", err)
	}
	config, err := crd.ConvertObject(collections.IstioSecurityV1Beta1Authorizationpolicies, item, "")
	if err != nil {
		return nil, fmt.Errorf("unable to convert yaml converted istio object to authorization policy model config, err: %s", err)
	}
	return config, nil
}

// FetchServicesFromDir walks through the files under a directory, based on the naming convention, it parses the service name
// from the file name.
func FetchServicesFromDir(namespace, localDirPath string) ([]string, error) {
	files, err := ioutil.ReadDir(localDirPath)
	if err != nil {
		return []string{}, fmt.Errorf("unable to read yaml files to local directory, err: %s", err)
	}

	var svcList []string
	for _, file := range files {
		if strings.Contains(file.Name(), namespace) {
			service := strings.Replace(file.Name(), "--"+namespace+".yaml", "", -1)
			svcList = append(svcList, service)
		}
	}
	return svcList, nil
}

type ComponentEnabled struct {
	serviceMap   map[string]bool
	namespaceMap map[string]bool
	cluster      bool
}

func ParseComponentsEnabledAuthzPolicy(componentsList string) (*ComponentEnabled, error) {
	componentEnabledObj := ComponentEnabled{}
	if componentsList == "" {
		return &componentEnabledObj, nil
	}
	serviceEnabledMap := make(map[string]bool)
	namespaceEnabledMap := make(map[string]bool)
	serviceNamespaceComboList := strings.Split(componentsList, ",")
	if len(serviceNamespaceComboList) == 1 && serviceNamespaceComboList[0] == "*" {
		componentEnabledObj.cluster = true
		return &componentEnabledObj, nil
	}
	for _, item := range serviceNamespaceComboList {
		if item != "" {
			serviceWithNS := strings.Split(item, "/")
			if len(serviceWithNS) != 2 {
				return nil, fmt.Errorf("service item %s from command line arg components-enabled-authzpolicy is in incorrect format", item)
			} else {
				if serviceWithNS[1] == "*" {
					namespaceEnabledMap[serviceWithNS[0]] = true
				} else {
					serviceEnabledMap[serviceWithNS[0]+"/"+serviceWithNS[1]] = true
				}
			}
		}
	}
	componentEnabledObj.serviceMap = serviceEnabledMap
	componentEnabledObj.namespaceMap = namespaceEnabledMap
	return &componentEnabledObj, nil
}

func (c *ComponentEnabled) containsService(service string, ns string) bool {
	_, exists := c.serviceMap[ns+"/"+service]
	return exists
}

func (c *ComponentEnabled) containsNamespace(ns string) bool {
	_, exists := c.namespaceMap[ns]
	return exists
}

func (c *ComponentEnabled) IsEnabled(serviceName string, serviceNamespace string) bool {
	if c.cluster {
		return true
	}
	if c.containsNamespace(serviceNamespace) {
		return true
	}
	if c.containsService(serviceName, serviceNamespace) {
		return true
	}
	return false
}
