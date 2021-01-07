// Copyright 2020 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Code generated by informer-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"
	time "time"

	idpv1alpha1 "go.pinniped.dev/generated/1.20/apis/supervisor/idp/v1alpha1"
	versioned "go.pinniped.dev/generated/1.20/client/supervisor/clientset/versioned"
	internalinterfaces "go.pinniped.dev/generated/1.20/client/supervisor/informers/externalversions/internalinterfaces"
	v1alpha1 "go.pinniped.dev/generated/1.20/client/supervisor/listers/idp/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
)

// OIDCIdentityProviderInformer provides access to a shared informer and lister for
// OIDCIdentityProviders.
type OIDCIdentityProviderInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1alpha1.OIDCIdentityProviderLister
}

type oIDCIdentityProviderInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewOIDCIdentityProviderInformer constructs a new informer for OIDCIdentityProvider type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewOIDCIdentityProviderInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredOIDCIdentityProviderInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredOIDCIdentityProviderInformer constructs a new informer for OIDCIdentityProvider type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredOIDCIdentityProviderInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.IDPV1alpha1().OIDCIdentityProviders(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.IDPV1alpha1().OIDCIdentityProviders(namespace).Watch(context.TODO(), options)
			},
		},
		&idpv1alpha1.OIDCIdentityProvider{},
		resyncPeriod,
		indexers,
	)
}

func (f *oIDCIdentityProviderInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredOIDCIdentityProviderInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *oIDCIdentityProviderInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&idpv1alpha1.OIDCIdentityProvider{}, f.defaultInformer)
}

func (f *oIDCIdentityProviderInformer) Lister() v1alpha1.OIDCIdentityProviderLister {
	return v1alpha1.NewOIDCIdentityProviderLister(f.Informer().GetIndexer())
}
