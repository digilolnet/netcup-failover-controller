package netcup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(srv *httptest.Server) *soapClient {
	return &soapClient{
		http:     srv.Client(),
		endpoint: srv.URL,
	}
}

// Real response shapes captured from the netcup SOAP API.
const realFaultInvalidCredentials = `<?xml version='1.0' encoding='UTF-8'?>
<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/">
  <S:Body>
    <S:Fault xmlns:ns4="http://www.w3.org/2003/05/soap-envelope">
      <faultcode>S:Server</faultcode>
      <faultstring>Invalid credentials.</faultstring>
    </S:Fault>
  </S:Body>
</S:Envelope>`

const realFaultFailoverIPNotFound = `<?xml version='1.0' encoding='UTF-8'?>
<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/">
  <S:Body>
    <S:Fault xmlns:ns4="http://www.w3.org/2003/05/soap-envelope">
      <faultcode>S:Server</faultcode>
      <faultstring>Could not find failover IP: 198.51.100.4 for User: 12345</faultstring>
    </S:Fault>
  </S:Body>
</S:Envelope>`

const realFaultServerNotFound = `<?xml version='1.0' encoding='UTF-8'?>
<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/">
  <S:Body>
    <S:Fault xmlns:ns4="http://www.w3.org/2003/05/soap-envelope">
      <faultcode>S:Server</faultcode>
      <faultstring>Server not found for name 'doesnotexist'</faultstring>
    </S:Fault>
  </S:Body>
</S:Envelope>`

// Real getVServerInformation response captured from the netcup SOAP API.
const realGetVServerInformationResponse = `<?xml version='1.0' encoding='UTF-8'?>
<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns1="http://enduser.service.web.vcp.netcup.de/">
  <S:Body>
    <ns1:getVServerInformationResponse>
      <return>
        <cpuCores>8</cpuCores>
        <ips>198.51.100.1</ips>
        <ips>2001:db8:1::/64</ips>
        <memory>16384</memory>
        <rebootRecommended>false</rebootRecommended>
        <rescueEnabled>false</rescueEnabled>
        <serverInterfaces>
          <driver>virtio</driver>
          <ipv4IP>198.51.100.1</ipv4IP>
          <ipv6IP>2001:db8:1::/64</ipv6IP>
          <mac>aa:bb:cc:dd:ee:ff</mac>
          <trafficThrottled>false</trafficThrottled>
        </serverInterfaces>
        <serverInterfaces>
          <driver>virtio</driver>
          <mac>aa:bb:cc:dd:ee:fe</mac>
          <trafficThrottled>false</trafficThrottled>
        </serverInterfaces>
        <status>online</status>
        <vServerName>v1234567890123456789</vServerName>
        <vServerNickname>example.server.invalid</vServerNickname>
      </return>
    </ns1:getVServerInformationResponse>
  </S:Body>
</S:Envelope>`

func serve(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(body))
	}))
}

func TestChangeIPRouting_Success(t *testing.T) {
	srv := serve(`<?xml version='1.0' encoding='UTF-8'?>
<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/">
  <S:Body>
    <ns2:changeIPRoutingResponse xmlns:ns2="http://enduser.service.web.vcp.netcup.de/"/>
  </S:Body>
</S:Envelope>`)
	defer srv.Close()

	err := newTestClient(srv).ChangeIPRouting(context.Background(), "12345", "pass", "198.51.100.2", "32", "v1234567890123456789", "aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestChangeIPRouting_FailoverIPNotFound(t *testing.T) {
	srv := serve(realFaultFailoverIPNotFound)
	defer srv.Close()

	err := newTestClient(srv).ChangeIPRouting(context.Background(), "12345", "pass", "198.51.100.4", "32", "v1234567890123456789", "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Could not find failover IP") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestChangeIPRouting_InvalidCredentials(t *testing.T) {
	srv := serve(realFaultInvalidCredentials)
	defer srv.Close()

	err := newTestClient(srv).ChangeIPRouting(context.Background(), "12345", "wrong", "198.51.100.2", "32", "v1234567890123456789", "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid credentials") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestChangeIPRouting_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	err := newTestClient(srv).ChangeIPRouting(context.Background(), "12345", "pass", "198.51.100.2", "32", "v1234567890123456789", "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestChangeIPRouting_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := newTestClient(srv).ChangeIPRouting(ctx, "12345", "pass", "198.51.100.2", "32", "v1234567890123456789", "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestChangeIPRouting_MalformedXML(t *testing.T) {
	srv := serve("not xml at all <<<")
	defer srv.Close()

	err := newTestClient(srv).ChangeIPRouting(context.Background(), "12345", "pass", "198.51.100.2", "32", "v1234567890123456789", "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetServerInfo_Success(t *testing.T) {
	srv := serve(realGetVServerInformationResponse)
	defer srv.Close()

	info, err := newTestClient(srv).GetServerInfo(context.Background(), "12345", "pass", "v1234567890123456789")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if info.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("expected MAC aa:bb:cc:dd:ee:ff, got: %s", info.MAC)
	}
}

func TestGetServerInfo_ServerNotFound(t *testing.T) {
	srv := serve(realFaultServerNotFound)
	defer srv.Close()

	_, err := newTestClient(srv).GetServerInfo(context.Background(), "12345", "pass", "doesnotexist")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Server not found") {
		t.Errorf("unexpected error: %v", err)
	}
}
