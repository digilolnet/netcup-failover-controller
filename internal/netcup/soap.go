package netcup

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultSOAPEndpoint = "https://www.servercontrolpanel.de/WSEndUser"
const soapNamespace = "http://enduser.service.web.vcp.netcup.de/"

type ServerInfo struct {
	MAC string // MAC of the primary interface (the one with an IPv4 address)
}

// SOAPAPI is the interface used by the controller so tests can inject a mock.
type SOAPAPI interface {
	ChangeIPRouting(ctx context.Context, login, password, failoverIP, netmask, serverName, macAddress string) error
	GetServerInfo(ctx context.Context, login, password, serverName string) (*ServerInfo, error)
}

type soapClient struct {
	http     *http.Client
	endpoint string
}

func NewSOAPClient() *soapClient {
	return &soapClient{
		http:     &http.Client{Timeout: 30 * time.Second},
		endpoint: defaultSOAPEndpoint,
	}
}

func (c *soapClient) send(ctx context.Context, body string) ([]byte, error) {
	envelope := fmt.Sprintf(`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ws="%s">
  <soapenv:Body>
    %s
  </soapenv:Body>
</soapenv:Envelope>`, soapNamespace, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewBufferString(envelope))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=UTF-8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SOAP HTTP %d: %s", resp.StatusCode, raw)
	}
	return raw, nil
}

func checkFault(raw []byte) error {
	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Fault *struct {
				FaultString string `xml:"faultstring"`
			} `xml:"Fault"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parsing SOAP response: %w", err)
	}
	if env.Body.Fault != nil {
		return fmt.Errorf("netcup SOAP fault: %s", env.Body.Fault.FaultString)
	}
	return nil
}

func (c *soapClient) ChangeIPRouting(ctx context.Context, login, password, failoverIP, netmask, serverName, macAddress string) error {
	body := fmt.Sprintf(`<ws:changeIPRouting>
      <loginName>%s</loginName>
      <password>%s</password>
      <routedIP>%s</routedIP>
      <routedMask>%s</routedMask>
      <destinationVserverName>%s</destinationVserverName>
      <destinationInterfaceMAC>%s</destinationInterfaceMAC>
    </ws:changeIPRouting>`,
		login, xmlEscape(password),
		failoverIP, netmask,
		serverName, macAddress,
	)
	raw, err := c.send(ctx, body)
	if err != nil {
		return err
	}
	return checkFault(raw)
}

func (c *soapClient) GetServerInfo(ctx context.Context, login, password, serverName string) (*ServerInfo, error) {
	body := fmt.Sprintf(`<ws:getVServerInformation>
      <loginName>%s</loginName>
      <password>%s</password>
      <vservername>%s</vservername>
    </ws:getVServerInformation>`,
		login, xmlEscape(password), serverName,
	)

	raw, err := c.send(ctx, body)
	if err != nil {
		return nil, err
	}
	if err := checkFault(raw); err != nil {
		return nil, err
	}

	var env struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			Inner struct {
				Return struct {
					Interfaces []struct {
						MAC     string   `xml:"mac"`
						IPv4IPs []string `xml:"ipv4IP"`
					} `xml:"serverInterfaces"`
				} `xml:"return"`
			} `xml:",any"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parsing getVServerInformation response: %w", err)
	}

	for _, iface := range env.Body.Inner.Return.Interfaces {
		if len(iface.IPv4IPs) > 0 {
			return &ServerInfo{MAC: iface.MAC}, nil
		}
	}
	return nil, fmt.Errorf("no interface with IPv4 found for server %q", serverName)
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
