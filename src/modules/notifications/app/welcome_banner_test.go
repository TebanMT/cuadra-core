package app

import (
	"testing"

	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
)

// El contrato de variables que arma buildVars DEBE cubrir exactamente las
// Variables declaradas en template.DefaultLibrary para cada template Media —
// si difieren, contentVariablesJSON dejaría una posición vacía y Twilio
// rechazaría con 21656. Este test ancla ambos lados juntos.
func TestBannerTemplates_VarsMatchLibraryContract(t *testing.T) {
	payload := map[string]string{
		"member_first_name": "Esteban",
		"full_name":         "Laura",
		"gym_name":          "Gym Bros",
		"member_number":     "4827",
	}
	const url = "https://media.entinta.mx/welcome/g/n.png"

	for key, spec := range bannerTemplates {
		def := tplDomain.LookupDefault(key)
		if def == nil {
			t.Fatalf("%s: no existe en DefaultLibrary", key)
		}
		vars := spec.buildVars(payload, url)
		if len(vars) != len(def.Variables) {
			t.Errorf("%s: buildVars dio %d vars, el template declara %d (%v vs %v)",
				key, len(vars), len(def.Variables), keysOf(vars), def.Variables)
		}
		for _, name := range def.Variables {
			if _, ok := vars[name]; !ok {
				t.Errorf("%s: la variable %q del template no la produce buildVars", key, name)
			}
		}
		if vars["welcome_image_url"] != url {
			t.Errorf("%s: welcome_image_url no es la URL del banner", key)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
