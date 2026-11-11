package dstack

// testServers builds a one-server registry for tests.
func testServers(baseURL string, backends ProviderBackends, mode ServerMode) Servers {
	servers, err := NewServers(Server{Name: "test", URL: baseURL, Token: "test-token", Mode: mode, Backends: backends})
	if err != nil {
		panic(err)
	}
	return servers
}

func testProxy(baseURL string, backends ProviderBackends, mode ServerMode, credentials CredentialSource) *Proxy {
	return NewProxy(testServers(baseURL, backends, mode), credentials, nil, nil)
}

func testServer(backends ProviderBackends, mode ServerMode) Server {
	return Server{Name: "test", URL: "https://dstack.invalid", Token: "test-token", Mode: mode, Backends: backends}
}
