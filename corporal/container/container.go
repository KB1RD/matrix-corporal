package container

import (
	"devture-matrix-corporal/corporal/avatar"
	"devture-matrix-corporal/corporal/configuration"
	"devture-matrix-corporal/corporal/connector"
	"devture-matrix-corporal/corporal/httpapi"
	"devture-matrix-corporal/corporal/httpapi/handler"
	"devture-matrix-corporal/corporal/httpgateway"
	"devture-matrix-corporal/corporal/matrix"
	"devture-matrix-corporal/corporal/policy"
	"devture-matrix-corporal/corporal/policy/provider"
	"devture-matrix-corporal/corporal/reconciliation/computator"
	"devture-matrix-corporal/corporal/reconciliation/reconciler"
	"devture-matrix-corporal/corporal/userauth"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	lru "github.com/hashicorp/golang-lru"

	"github.com/euskadi31/go-service"
	"github.com/sirupsen/logrus"
)

type ContainerShutdownHandler struct {
	destructors []func()
}

func (me *ContainerShutdownHandler) Add(destructor func()) {
	me.destructors = append(me.destructors, destructor)
}

func (me *ContainerShutdownHandler) Shutdown() {
	for i, _ := range me.destructors {
		me.destructors[len(me.destructors)-i-1]()
	}
}

func BuildContainer(
	configuration configuration.Configuration,
) (service.Container, *ContainerShutdownHandler) {
	container := service.New()
	shutdownHandler := &ContainerShutdownHandler{}

	// The logger is very crucial, so we're defining it outside
	logger := logrus.New()
	if configuration.Misc.Debug {
		logger.Level = logrus.DebugLevel
	}

	container.Set("logger", func(c service.Container) interface{} {
		return logger
	})

	container.Set("matrix.user_mapping_resolver.cache", func(c service.Container) interface{} {
		cache, err := lru.New2Q(1000)
		if err != nil {
			panic(err)
		}
		return cache
	})

	container.Set("matrix.user_mapping_resolver", func(c service.Container) interface{} {
		return matrix.NewUserMappingResolver(
			logger,
			container.Get("matrix.user_mapping_resolver.cache").(*lru.TwoQueueCache),
			configuration.Matrix.HomeserverApiEndpoint,
		)
	})

	container.Set("matrix.http_reverse_proxy", func(c service.Container) interface{} {
		u, _ := url.Parse(configuration.Matrix.HomeserverApiEndpoint)
		reverseProxy := httputil.NewSingleHostReverseProxy(u)

		// To control the timeout, we need to use our own transport.
		reverseProxy.Transport = &http.Transport{
			ResponseHeaderTimeout: time.Duration(configuration.Matrix.TimeoutMilliseconds) * time.Millisecond,

			// For other options, we stick to the defaults
			Proxy:                 http.DefaultTransport.(*http.Transport).Proxy,
			DialContext:           http.DefaultTransport.(*http.Transport).DialContext,
			MaxIdleConns:          http.DefaultTransport.(*http.Transport).MaxIdleConns,
			IdleConnTimeout:       http.DefaultTransport.(*http.Transport).IdleConnTimeout,
			TLSHandshakeTimeout:   http.DefaultTransport.(*http.Transport).TLSHandshakeTimeout,
			ExpectContinueTimeout: http.DefaultTransport.(*http.Transport).ExpectContinueTimeout,
		}

		reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Errorf("HTTP Reverse Proxy: failed proxying [%s] %s: %s", r.Method, r.URL, err)
			w.WriteHeader(http.StatusBadGateway)
		}

		return reverseProxy
	})

	container.Set("matrix.shared_secret_auth.password_generator", func(c service.Container) interface{} {
		return matrix.NewSharedSecretAuthPasswordGenerator(configuration.Matrix.AuthSharedSecret)
	})

	container.Set("httpgateway.interceptor.login", func(c service.Container) interface{} {
		return httpgateway.NewLoginInterceptor(
			container.Get("policy.store").(*policy.Store),
			configuration.Matrix.HomeserverDomainName,
			container.Get("policy.userauth.checker").(*userauth.Checker),
			container.Get("matrix.shared_secret_auth.password_generator").(*matrix.SharedSecretAuthPasswordGenerator),
		)
	})

	container.Set("httpgateway.interceptor.uiauth", func(c service.Container) interface{} {
		return httpgateway.NewUiAuthInterceptor(
			container.Get("policy.store").(*policy.Store),
			configuration.Matrix.HomeserverDomainName,
			container.Get("policy.userauth.checker").(*userauth.Checker),
			container.Get("matrix.shared_secret_auth.password_generator").(*matrix.SharedSecretAuthPasswordGenerator),
		)
	})

	container.Set("httpgateway.server", func(c service.Container) interface{} {
		instance := httpgateway.NewServer(
			logger,
			configuration.HttpGateway,
			container.Get("matrix.http_reverse_proxy").(*httputil.ReverseProxy),
			container.Get("matrix.user_mapping_resolver").(*matrix.UserMappingResolver),
			container.Get("policy.store").(*policy.Store),
			container.Get("policy.checker").(*policy.Checker),
			container.Get("httpgateway.interceptor.login").(httpgateway.Interceptor),
			container.Get("httpgateway.interceptor.uiauth").(httpgateway.Interceptor),
			time.Duration(configuration.HttpGateway.TimeoutMilliseconds)*time.Millisecond,
		)

		shutdownHandler.Add(func() {
			instance.Stop()
		})

		return instance
	})

	container.Set("httpapi.server", func(c service.Container) interface{} {
		instance := httpapi.NewServer(
			logger,
			configuration.HttpApi,
			container.Get("httpapi.server.handler_registrators").([]handler.HandlerRegistrator),
			time.Duration(configuration.HttpApi.TimeoutMilliseconds)*time.Millisecond,
		)

		shutdownHandler.Add(func() {
			instance.Stop()
		})

		return instance
	})

	container.Set("httpapi.server.handler_registrators", func(c service.Container) interface{} {
		return []handler.HandlerRegistrator{
			container.Get("httpapi.server.handler_registrator.policy").(handler.HandlerRegistrator),
			container.Get("httpapi.server.handler_registrator.user").(handler.HandlerRegistrator),
		}
	})

	container.Set("httpapi.server.handler_registrator.policy", func(c service.Container) interface{} {
		return handler.NewPolicyApiHandlerRegistrator(
			container.Get("policy.store").(*policy.Store),
			container.Get("policy.provider").(provider.Provider),
		)
	})

	container.Set("httpapi.server.handler_registrator.user", func(c service.Container) interface{} {
		return handler.NewUserApiHandlerRegistrator(
			configuration.Matrix.HomeserverDomainName,
			container.Get("connector.synapse").(*connector.SynapseConnector),
		)
	})

	container.Set("policy.store", func(c service.Container) interface{} {
		return policy.NewStore(
			logger,
			container.Get("policy.validator").(*policy.Validator),
		)
	})

	container.Set("policy.checker", func(c service.Container) interface{} {
		return policy.NewChecker()
	})

	container.Set("policy.validator", func(c service.Container) interface{} {
		return policy.NewValidator(
			configuration.Matrix.HomeserverDomainName,
		)
	})

	container.Set("matrix.userauth.rest_cache", func(c service.Container) interface{} {
		cache, err := lru.New(1000)
		if err != nil {
			panic(err)
		}
		return cache
	})

	container.Set("policy.userauth.checker", func(c service.Container) interface{} {
		instance := userauth.NewChecker()

		instance.RegisterAuthenticator(userauth.NewPlainAuthenticator())
		instance.RegisterAuthenticator(userauth.NewMd5Authenticator())
		instance.RegisterAuthenticator(userauth.NewSha1Authenticator())
		instance.RegisterAuthenticator(userauth.NewSha256Authenticator())
		instance.RegisterAuthenticator(userauth.NewSha512Authenticator())
		instance.RegisterAuthenticator(userauth.NewBcryptAuthenticator())

		restAuthenticator := userauth.NewRestAuthenticator()
		instance.RegisterAuthenticator(restAuthenticator)
		instance.RegisterAuthenticator(userauth.NewCacheFallackAuthenticator(
			"rest-with-cache-fallback",
			restAuthenticator,
			container.Get("matrix.userauth.rest_cache").(*lru.Cache),
			logger,
		))

		return instance
	})

	container.Set("policy.provider", func(c service.Container) interface{} {
		instance, err := provider.CreateProviderByConfig(
			configuration.PolicyProvider,
			container.Get("policy.store").(*policy.Store),
			logger,
		)

		if err != nil {
			panic(err)
		}

		shutdownHandler.Add(func() {
			instance.Stop()
		})

		return instance
	})

	container.Set("avatar.avatar_reader", func(c service.Container) interface{} {
		return avatar.NewAvatarReader()
	})

	container.Set("reconciliation.computator", func(c service.Container) interface{} {
		return computator.NewReconciliationStateComputator(
			logger,
		)
	})

	container.Set("reconciliation.reconciler", func(c service.Container) interface{} {
		return reconciler.New(
			logger,
			container.Get("connector.synapse").(*connector.SynapseConnector),
			container.Get("reconciliation.computator").(*computator.ReconciliationStateComputator),
			configuration.Reconciliation.UserId,
			container.Get("avatar.avatar_reader").(*avatar.AvatarReader),
		)
	})

	container.Set("reconciliation.store_driven_reconciler", func(c service.Container) interface{} {
		instance := reconciler.NewStoreDrivenReconciler(
			logger,
			container.Get("policy.store").(*policy.Store),
			container.Get("reconciliation.reconciler").(*reconciler.Reconciler),
			configuration.Reconciliation.RetryIntervalMilliseconds,
		)

		shutdownHandler.Add(func() {
			instance.Stop()
		})

		return instance
	})

	container.Set("connector.api", func(c service.Container) interface{} {
		return connector.NewApiConnector(
			configuration.Matrix.HomeserverApiEndpoint,
			container.Get("matrix.shared_secret_auth.password_generator").(*matrix.SharedSecretAuthPasswordGenerator),
			configuration.Matrix.TimeoutMilliseconds,
			logger,
		)
	})

	container.Set("connector.synapse", func(c service.Container) interface{} {
		return connector.NewSynapseConnector(
			container.Get("connector.api").(*connector.ApiConnector),
			configuration.Matrix.RegistrationSharedSecret,
		)
	})

	return container, shutdownHandler
}
