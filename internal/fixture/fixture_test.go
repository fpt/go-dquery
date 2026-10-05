package fixture

import (
	"os"
	"strings"
	"testing"
)

// The example schema used by cmd/dq and the README must match the fixture.
func TestExampleSchemaInSync(t *testing.T) {
	b, err := os.ReadFile("../../examples/shop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var body []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "#") {
			body = append(body, l)
		}
	}
	if strings.Join(body, "\n") != ShopYAML[1:] {
		t.Fatal("examples/shop.yaml differs from fixture.ShopYAML; update one to match the other")
	}
}
