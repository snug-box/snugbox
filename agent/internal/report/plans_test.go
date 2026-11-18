package report

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlansGolden(t *testing.T) {
	r := shopResult(t)
	var buf bytes.Buffer
	if err := Plans(&buf, r, "", Painter{}); err != nil {
		t.Fatal(err)
	}
	golden(t, "shop.plans.golden", buf.Bytes())

	buf.Reset()
	if err := Plans(&buf, r, "hetzner", Painter{}); err != nil {
		t.Fatal(err)
	}
	golden(t, "shop.plans-hetzner.golden", buf.Bytes())
	out := buf.String()
	sold, gone := strings.Index(out, "PLANS THAT FIT"), strings.Index(out, "NOT AVAILABLE TO ORDER")
	if sold < 0 || gone < sold {
		t.Errorf("plans that can't be ordered must follow the ranked table, not mix with it:\n%s", out)
	}
	if strings.Contains(out[sold:gone], "CX23") || !strings.Contains(out[gone:], "CX23") {
		t.Errorf("the unavailable CX23 is ranked with plans that can be bought:\n%s", out)
	}
	if strings.Contains(out[gone:], "$40.91") {
		t.Errorf("a plan that can't be ordered shows a saving:\n%s", out[gone:])
	}
	if strings.Contains(out, "DigitalOcean") {
		t.Errorf("Hetzner's plans came with DigitalOcean's resize guide:\n%s", out)
	}
}

func TestPlansRefusals(t *testing.T) {
	r := shopResult(t)
	var buf bytes.Buffer
	if err := Plans(&buf, r, "nosuch", Painter{}); err == nil || !strings.Contains(err.Error(), `unknown provider "nosuch"; the catalogue has `) {
		t.Errorf("Plans(nosuch) = %v", err)
	}

	r.Advice.Withheld = true
	buf.Reset()
	if err := Plans(&buf, r, "", Painter{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No smaller plan is advised") || strings.Contains(buf.String(), "PLANS THAT FIT") {
		t.Errorf("a box that needs more memory was offered plans:\n%s", buf.String())
	}

	r.Advice.Withheld, r.Advice.Catalogue = false, nil
	buf.Reset()
	if err := Plans(&buf, r, "", Painter{}); err != nil || !strings.Contains(buf.String(), "The price catalogue could not be read.") {
		t.Errorf("no catalogue: %v\n%s", err, buf.String())
	}
}
