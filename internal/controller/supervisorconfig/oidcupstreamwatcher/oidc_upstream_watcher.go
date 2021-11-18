// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oidcupstreamwatcher implements a controller which watches OIDCIdentityProviders.
package oidcupstreamwatcher

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/cache"
	corev1informers "k8s.io/client-go/informers/core/v1"

	"go.pinniped.dev/generated/latest/apis/supervisor/idp/v1alpha1"
	pinnipedclientset "go.pinniped.dev/generated/latest/client/supervisor/clientset/versioned"
	idpinformers "go.pinniped.dev/generated/latest/client/supervisor/informers/externalversions/idp/v1alpha1"
	"go.pinniped.dev/internal/constable"
	pinnipedcontroller "go.pinniped.dev/internal/controller"
	"go.pinniped.dev/internal/controller/conditionsutil"
	"go.pinniped.dev/internal/controller/supervisorconfig/upstreamwatchers"
	"go.pinniped.dev/internal/controllerlib"
	"go.pinniped.dev/internal/net/phttp"
	"go.pinniped.dev/internal/oidc/provider"
	"go.pinniped.dev/internal/upstreamoidc"
)

const (
	// Setup for the name of our controller in logs.
	oidcControllerName = "oidc-upstream-observer"

	// Constants related to the client credentials Secret.
	oidcClientSecretType corev1.SecretType = "secrets.pinniped.dev/oidc-client"

	clientIDDataKey     = "clientID"
	clientSecretDataKey = "clientSecret"

	// Constants related to the OIDC provider discovery cache. These do not affect the cache of JWKS.
	oidcValidatorCacheTTL = 15 * time.Minute

	// Constants related to conditions.
	typeClientCredentialsValid             = "ClientCredentialsValid"
	typeAdditionalAuthorizeParametersValid = "AdditionalAuthorizeParametersValid"
	typeOIDCDiscoverySucceeded             = "OIDCDiscoverySucceeded"

	reasonUnreachable             = "Unreachable"
	reasonInvalidResponse         = "InvalidResponse"
	reasonDisallowedParameterName = "DisallowedParameterName"
	allParamNamesAllowedMsg       = "additionalAuthorizeParameters parameter names are allowed"

	// Errors that are generated by our reconcile process.
	errOIDCFailureStatus = constable.Error("OIDCIdentityProvider has a failing condition")
)

var (
	disallowedAdditionalAuthorizeParameters = map[string]bool{ //nolint: gochecknoglobals
		// Reject these AdditionalAuthorizeParameters to avoid allowing the user's config to overwrite the parameters
		// that are always used by Pinniped in authcode authorization requests. The OIDC library used would otherwise
		// happily treat the user's config as an override. Users can already set the "client_id" and "scope" params
		// using other settings, and the others never make sense to override. This map should be treated as read-only
		// since it is a global variable.
		"response_type":         true,
		"scope":                 true,
		"client_id":             true,
		"state":                 true,
		"nonce":                 true,
		"code_challenge":        true,
		"code_challenge_method": true,
		"redirect_uri":          true,

		// Reject "hd" for now because it is not safe to use with Google's OIDC provider until Pinniped also
		// performs the corresponding validation on the ID token.
		"hd": true,
	}
)

// UpstreamOIDCIdentityProviderICache is a thread safe cache that holds a list of validated upstream OIDC IDP configurations.
type UpstreamOIDCIdentityProviderICache interface {
	SetOIDCIdentityProviders([]provider.UpstreamOIDCIdentityProviderI)
}

// lruValidatorCache caches the *oidc.Provider associated with a particular issuer/TLS configuration.
type lruValidatorCache struct{ cache *cache.Expiring }

type lruValidatorCacheEntry struct {
	provider *oidc.Provider
	client   *http.Client
}

func (c *lruValidatorCache) getProvider(spec *v1alpha1.OIDCIdentityProviderSpec) (*oidc.Provider, *http.Client) {
	if result, ok := c.cache.Get(c.cacheKey(spec)); ok {
		entry := result.(*lruValidatorCacheEntry)
		return entry.provider, entry.client
	}
	return nil, nil
}

func (c *lruValidatorCache) putProvider(spec *v1alpha1.OIDCIdentityProviderSpec, provider *oidc.Provider, client *http.Client) {
	c.cache.Set(c.cacheKey(spec), &lruValidatorCacheEntry{provider: provider, client: client}, oidcValidatorCacheTTL)
}

func (c *lruValidatorCache) cacheKey(spec *v1alpha1.OIDCIdentityProviderSpec) interface{} {
	var key struct{ issuer, caBundle string }
	key.issuer = spec.Issuer
	if spec.TLS != nil {
		key.caBundle = spec.TLS.CertificateAuthorityData
	}
	return key
}

type oidcWatcherController struct {
	cache                        UpstreamOIDCIdentityProviderICache
	log                          logr.Logger
	client                       pinnipedclientset.Interface
	oidcIdentityProviderInformer idpinformers.OIDCIdentityProviderInformer
	secretInformer               corev1informers.SecretInformer
	validatorCache               interface {
		getProvider(*v1alpha1.OIDCIdentityProviderSpec) (*oidc.Provider, *http.Client)
		putProvider(*v1alpha1.OIDCIdentityProviderSpec, *oidc.Provider, *http.Client)
	}
}

// New instantiates a new controllerlib.Controller which will populate the provided UpstreamOIDCIdentityProviderICache.
func New(
	idpCache UpstreamOIDCIdentityProviderICache,
	client pinnipedclientset.Interface,
	oidcIdentityProviderInformer idpinformers.OIDCIdentityProviderInformer,
	secretInformer corev1informers.SecretInformer,
	log logr.Logger,
	withInformer pinnipedcontroller.WithInformerOptionFunc,
) controllerlib.Controller {
	c := oidcWatcherController{
		cache:                        idpCache,
		log:                          log.WithName(oidcControllerName),
		client:                       client,
		oidcIdentityProviderInformer: oidcIdentityProviderInformer,
		secretInformer:               secretInformer,
		validatorCache:               &lruValidatorCache{cache: cache.NewExpiring()},
	}
	return controllerlib.New(
		controllerlib.Config{Name: oidcControllerName, Syncer: &c},
		withInformer(
			oidcIdentityProviderInformer,
			pinnipedcontroller.MatchAnythingFilter(pinnipedcontroller.SingletonQueue()),
			controllerlib.InformerOption{},
		),
		withInformer(
			secretInformer,
			pinnipedcontroller.MatchAnySecretOfTypeFilter(oidcClientSecretType, pinnipedcontroller.SingletonQueue()),
			controllerlib.InformerOption{},
		),
	)
}

// Sync implements controllerlib.Syncer.
func (c *oidcWatcherController) Sync(ctx controllerlib.Context) error {
	actualUpstreams, err := c.oidcIdentityProviderInformer.Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list OIDCIdentityProviders: %w", err)
	}

	requeue := false
	validatedUpstreams := make([]provider.UpstreamOIDCIdentityProviderI, 0, len(actualUpstreams))
	for _, upstream := range actualUpstreams {
		valid := c.validateUpstream(ctx, upstream)
		if valid == nil {
			requeue = true
		} else {
			validatedUpstreams = append(validatedUpstreams, provider.UpstreamOIDCIdentityProviderI(valid))
		}
	}
	c.cache.SetOIDCIdentityProviders(validatedUpstreams)
	if requeue {
		return controllerlib.ErrSyntheticRequeue
	}
	return nil
}

// validateUpstream validates the provided v1alpha1.OIDCIdentityProvider and returns the validated configuration as a
// provider.UpstreamOIDCIdentityProvider. As a side effect, it also updates the status of the v1alpha1.OIDCIdentityProvider.
func (c *oidcWatcherController) validateUpstream(ctx controllerlib.Context, upstream *v1alpha1.OIDCIdentityProvider) *upstreamoidc.ProviderConfig {
	authorizationConfig := upstream.Spec.AuthorizationConfig

	additionalAuthcodeAuthorizeParameters := map[string]string{}
	var rejectedAuthcodeAuthorizeParameters []string
	for _, p := range authorizationConfig.AdditionalAuthorizeParameters {
		if disallowedAdditionalAuthorizeParameters[p.Name] {
			rejectedAuthcodeAuthorizeParameters = append(rejectedAuthcodeAuthorizeParameters, p.Name)
		} else {
			additionalAuthcodeAuthorizeParameters[p.Name] = p.Value
		}
	}

	result := upstreamoidc.ProviderConfig{
		Name: upstream.Name,
		Config: &oauth2.Config{
			Scopes: computeScopes(authorizationConfig.AdditionalScopes),
		},
		UsernameClaim:            upstream.Spec.Claims.Username,
		GroupsClaim:              upstream.Spec.Claims.Groups,
		AllowPasswordGrant:       authorizationConfig.AllowPasswordGrant,
		AdditionalAuthcodeParams: additionalAuthcodeAuthorizeParameters,
		ResourceUID:              upstream.UID,
	}

	conditions := []*v1alpha1.Condition{
		c.validateSecret(upstream, &result),
		c.validateIssuer(ctx.Context, upstream, &result),
	}
	if len(rejectedAuthcodeAuthorizeParameters) > 0 {
		conditions = append(conditions, &v1alpha1.Condition{
			Type:   typeAdditionalAuthorizeParametersValid,
			Status: v1alpha1.ConditionFalse,
			Reason: reasonDisallowedParameterName,
			Message: fmt.Sprintf("the following additionalAuthorizeParameters are not allowed: %s",
				strings.Join(rejectedAuthcodeAuthorizeParameters, ",")),
		})
	} else {
		conditions = append(conditions, &v1alpha1.Condition{
			Type:    typeAdditionalAuthorizeParametersValid,
			Status:  v1alpha1.ConditionTrue,
			Reason:  upstreamwatchers.ReasonSuccess,
			Message: allParamNamesAllowedMsg,
		})
	}

	c.updateStatus(ctx.Context, upstream, conditions)

	valid := true
	log := c.log.WithValues("namespace", upstream.Namespace, "name", upstream.Name)
	for _, condition := range conditions {
		if condition.Status == v1alpha1.ConditionFalse {
			valid = false
			log.WithValues(
				"type", condition.Type,
				"reason", condition.Reason,
				"message", condition.Message,
			).Error(errOIDCFailureStatus, "found failing condition")
		}
	}
	if valid {
		return &result
	}
	return nil
}

// validateSecret validates the .spec.client.secretName field and returns the appropriate ClientCredentialsValid condition.
func (c *oidcWatcherController) validateSecret(upstream *v1alpha1.OIDCIdentityProvider, result *upstreamoidc.ProviderConfig) *v1alpha1.Condition {
	secretName := upstream.Spec.Client.SecretName

	// Fetch the Secret from informer cache.
	secret, err := c.secretInformer.Lister().Secrets(upstream.Namespace).Get(secretName)
	if err != nil {
		return &v1alpha1.Condition{
			Type:    typeClientCredentialsValid,
			Status:  v1alpha1.ConditionFalse,
			Reason:  upstreamwatchers.ReasonNotFound,
			Message: err.Error(),
		}
	}

	// Validate the secret .type field.
	if secret.Type != oidcClientSecretType {
		return &v1alpha1.Condition{
			Type:    typeClientCredentialsValid,
			Status:  v1alpha1.ConditionFalse,
			Reason:  upstreamwatchers.ReasonWrongType,
			Message: fmt.Sprintf("referenced Secret %q has wrong type %q (should be %q)", secretName, secret.Type, oidcClientSecretType),
		}
	}

	// Validate the secret .data field.
	clientID := secret.Data[clientIDDataKey]
	clientSecret := secret.Data[clientSecretDataKey]
	if len(clientID) == 0 || len(clientSecret) == 0 {
		return &v1alpha1.Condition{
			Type:    typeClientCredentialsValid,
			Status:  v1alpha1.ConditionFalse,
			Reason:  upstreamwatchers.ReasonMissingKeys,
			Message: fmt.Sprintf("referenced Secret %q is missing required keys %q", secretName, []string{clientIDDataKey, clientSecretDataKey}),
		}
	}

	// If everything is valid, update the result and set the condition to true.
	result.Config.ClientID = string(clientID)
	result.Config.ClientSecret = string(clientSecret)
	return &v1alpha1.Condition{
		Type:    typeClientCredentialsValid,
		Status:  v1alpha1.ConditionTrue,
		Reason:  upstreamwatchers.ReasonSuccess,
		Message: "loaded client credentials",
	}
}

// validateIssuer validates the .spec.issuer field, performs OIDC discovery, and returns the appropriate OIDCDiscoverySucceeded condition.
func (c *oidcWatcherController) validateIssuer(ctx context.Context, upstream *v1alpha1.OIDCIdentityProvider, result *upstreamoidc.ProviderConfig) *v1alpha1.Condition {
	// Get the provider and HTTP Client from cache if possible.
	discoveredProvider, httpClient := c.validatorCache.getProvider(&upstream.Spec)

	// If the provider does not exist in the cache, do a fresh discovery lookup and save to the cache.
	if discoveredProvider == nil {
		var err error
		httpClient, err = getClient(upstream)
		if err != nil {
			return &v1alpha1.Condition{
				Type:    typeOIDCDiscoverySucceeded,
				Status:  v1alpha1.ConditionFalse,
				Reason:  upstreamwatchers.ReasonInvalidTLSConfig,
				Message: err.Error(),
			}
		}

		discoveredProvider, err = oidc.NewProvider(oidc.ClientContext(ctx, httpClient), upstream.Spec.Issuer)
		if err != nil {
			const klogLevelTrace = 6
			c.log.V(klogLevelTrace).WithValues(
				"namespace", upstream.Namespace,
				"name", upstream.Name,
				"issuer", upstream.Spec.Issuer,
			).Error(err, "failed to perform OIDC discovery")
			return &v1alpha1.Condition{
				Type:    typeOIDCDiscoverySucceeded,
				Status:  v1alpha1.ConditionFalse,
				Reason:  reasonUnreachable,
				Message: fmt.Sprintf("failed to perform OIDC discovery against %q:\n%s", upstream.Spec.Issuer, truncateMostLongErr(err)),
			}
		}

		// Update the cache with the newly discovered value.
		c.validatorCache.putProvider(&upstream.Spec, discoveredProvider, httpClient)
	}

	// Parse out and validate the discovered authorize endpoint.
	authURL, err := url.Parse(discoveredProvider.Endpoint().AuthURL)
	if err != nil {
		return &v1alpha1.Condition{
			Type:    typeOIDCDiscoverySucceeded,
			Status:  v1alpha1.ConditionFalse,
			Reason:  reasonInvalidResponse,
			Message: fmt.Sprintf("failed to parse authorization endpoint URL: %v", err),
		}
	}
	if authURL.Scheme != "https" {
		return &v1alpha1.Condition{
			Type:    typeOIDCDiscoverySucceeded,
			Status:  v1alpha1.ConditionFalse,
			Reason:  reasonInvalidResponse,
			Message: fmt.Sprintf(`authorization endpoint URL scheme must be "https", not %q`, authURL.Scheme),
		}
	}

	// If everything is valid, update the result and set the condition to true.
	result.Config.Endpoint = discoveredProvider.Endpoint()
	result.Provider = discoveredProvider
	result.Client = httpClient
	return &v1alpha1.Condition{
		Type:    typeOIDCDiscoverySucceeded,
		Status:  v1alpha1.ConditionTrue,
		Reason:  upstreamwatchers.ReasonSuccess,
		Message: "discovered issuer configuration",
	}
}

func (c *oidcWatcherController) updateStatus(ctx context.Context, upstream *v1alpha1.OIDCIdentityProvider, conditions []*v1alpha1.Condition) {
	log := c.log.WithValues("namespace", upstream.Namespace, "name", upstream.Name)
	updated := upstream.DeepCopy()

	hadErrorCondition := conditionsutil.Merge(conditions, upstream.Generation, &updated.Status.Conditions, log)

	updated.Status.Phase = v1alpha1.PhaseReady
	if hadErrorCondition {
		updated.Status.Phase = v1alpha1.PhaseError
	}

	if equality.Semantic.DeepEqual(upstream, updated) {
		return
	}

	_, err := c.client.
		IDPV1alpha1().
		OIDCIdentityProviders(upstream.Namespace).
		UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		log.Error(err, "failed to update status")
	}
}

func getClient(upstream *v1alpha1.OIDCIdentityProvider) (*http.Client, error) {
	if upstream.Spec.TLS == nil || upstream.Spec.TLS.CertificateAuthorityData == "" {
		return defaultClientShortTimeout(nil), nil
	}

	bundle, err := base64.StdEncoding.DecodeString(upstream.Spec.TLS.CertificateAuthorityData)
	if err != nil {
		return nil, fmt.Errorf("spec.certificateAuthorityData is invalid: %w", err)
	}

	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("spec.certificateAuthorityData is invalid: %w", upstreamwatchers.ErrNoCertificates)
	}

	return defaultClientShortTimeout(rootCAs), nil
}

func defaultClientShortTimeout(rootCAs *x509.CertPool) *http.Client {
	c := phttp.Default(rootCAs)
	c.Timeout = time.Minute
	return c
}

func computeScopes(additionalScopes []string) []string {
	// If none are set then provide a reasonable default which only tries to use scopes defined in the OIDC spec.
	if len(additionalScopes) == 0 {
		return []string{"openid", "offline_access", "email", "profile"}
	}

	// Otherwise, first compute the unique set of scopes, including "openid" (de-duplicate).
	set := sets.NewString()
	set.Insert("openid")
	for _, s := range additionalScopes {
		set.Insert(s)
	}

	// Return the set as a sorted list.
	return set.List()
}

func truncateMostLongErr(err error) string {
	const max = 300
	msg := err.Error()

	// always log oidc and x509 errors completely
	if len(msg) <= max || strings.Contains(msg, "oidc:") || strings.Contains(msg, "x509:") {
		return msg
	}

	return msg[:max] + fmt.Sprintf(" [truncated %d chars]", len(msg)-max)
}
