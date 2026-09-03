package config

import "testing"

func TestValidateNil(t *testing.T) {
	var c *Cfg
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for nil cfg")
	}
}

func TestValidatePortConflict(t *testing.T) {
	c := &Cfg{
		Web:     &WebConfig{HTTPPort: 8000},
		Ws:      &WebsocketConfig{Port: []*WebsocketPort{{Port: 8000}}},
		LogLevel: "info",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected port conflict")
	}
}

func TestValidateOK(t *testing.T) {
	c := &Cfg{
		Web: &WebConfig{HTTPPort: 1000},
		Ws: &WebsocketConfig{
			Port:           []*WebsocketPort{{Port: 8000}},
			CheckOrigin:    true,
			AllowedOrigins: []string{"http://127.0.0.1:3000"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
