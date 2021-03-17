package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform/repl"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	k8syaml "sigs.k8s.io/yaml"

	flag "github.com/spf13/pflag"
)

var toolVersion string

const resourceType = "kubernetes_manifest"

// NOTE The terraform console formatter only supports map[string]interface{}
// but the yaml parser spits out map[interface{}]interface{} so we need to convert

func fixSlice(s []interface{}) []interface{} {
	fixed := []interface{}{}

	for _, v := range s {
		switch v.(type) {
		case map[interface{}]interface{}:
			fixed = append(fixed, fixMap(v.(map[interface{}]interface{})))
		case []interface{}:
			fixed = append(fixed, fixSlice(v.([]interface{})))
		default:
			fixed = append(fixed, v)
		}
	}

	return fixed
}

func fixMap(m map[interface{}]interface{}) map[string]interface{} {
	fixed := map[string]interface{}{}

	for k, v := range m {
		switch v.(type) {
		case map[interface{}]interface{}:
			fixed[k.(string)] = fixMap(v.(map[interface{}]interface{}))
		case []interface{}:
			fixed[k.(string)] = fixSlice(v.([]interface{}))
		default:
			fixed[k.(string)] = v
		}
	}

	return fixed
}

var serverSideMetadataFields = []string{
	"creationTimestamp",
	"resourceVersion",
	"selfLink",
	"uid",
	"managedFields",
	"finalizers",
}

func stripServerSideFields(doc cty.Value) cty.Value {
	m := doc.AsValueMap()

	// strip server-side metadata
	metadata := m["metadata"].AsValueMap()
	for _, f := range serverSideMetadataFields {
		delete(metadata, f)
	}
	if v, ok := metadata["annotations"]; ok {
		annotations := v.AsValueMap()
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		if len(annotations) == 0 {
			delete(metadata, "annotations")
		} else {
			metadata["annotations"] = cty.ObjectVal(annotations)
		}
	}
	if ns, ok := metadata["namespace"]; ok && ns.AsString() == "default" {
		delete(metadata, "namespace")
	}
	m["metadata"] = cty.ObjectVal(metadata)

	// strip finalizer from spec
	if v, ok := m["spec"]; ok {
		mm := v.AsValueMap()
		delete(mm, "finalizers")
		m["spec"] = cty.ObjectVal(mm)
	}

	// strip status field
	delete(m, "status")

	return cty.ObjectVal(m)
}

func toHCL(doc cty.Value, providerAlias string, stripServerSide bool, mapOnly bool) (string, error) {
	var name, resourceName string
	m := doc.AsValueMap()
	kind := m["kind"].AsString()
	if kind != "List" {
		metadata := m["metadata"].AsValueMap()
		name = metadata["name"].AsString()
		re := regexp.MustCompile(`\W`)
		name = strings.ToLower(re.ReplaceAllString(name, "_"))
		resourceName = strings.ToLower(kind) + "_" + name
	} else if !mapOnly {
		return "", fmt.Errorf("Converting v1.List to a full Terraform configuation is currently not supported")
	}

	if stripServerSide {
		doc = stripServerSideFields(doc)
	}
	s := repl.FormatValue(doc, 0)

	var hcl string
	if mapOnly {
		hcl = fmt.Sprintf("%v\n", s)
	} else {
		hcl = fmt.Sprintf("resource %q %q {\n", resourceType, resourceName)
		if providerAlias != "" {
			hcl += fmt.Sprintf("  provider = %v\n\n", providerAlias)
		}
		hcl += fmt.Sprintf("  manifest = %v\n", strings.ReplaceAll(s, "\n", "\n  "))
		hcl += fmt.Sprintf("}\n")
	}

	return hcl, nil
}

var yamlSeparator = "\n---"

// ToHCL converts a file containing one or more Kubernetes configs
// and converts it to resources that can be used by the Terraform Kubernetes Provider
func ToHCL(r io.Reader, providerAlias string, stripServerSide bool, mapOnly bool) (string, error) {
	hcl := ""

	buf := bytes.Buffer{}
	_, err := buf.ReadFrom(r)
	if err != nil {
		return "", err
	}

	count := 0
	manifest := string(buf.Bytes())
	docs := strings.Split(manifest, yamlSeparator)
	for _, doc := range docs {
		var b []byte
		b, err = k8syaml.YAMLToJSON([]byte(doc))
		if err != nil {
			return "", err
		}

		t, err := ctyjson.ImpliedType(b)
		if err != nil {
			return "", err
		}

		doc, err := ctyjson.Unmarshal(b, t)
		if err != nil {
			return "", err
		}

		formatted, err := toHCL(doc, providerAlias, stripServerSide, mapOnly)

		if err != nil {
			return "", fmt.Errorf("error converting YAML to HCL: %s", err)
		}

		if count > 0 {
			hcl += "\n"
		}
		hcl += formatted
		count++
	}

	return hcl, nil
}

func main() {
	infile := flag.StringP("file", "f", "-", "Input file containing Kubernetes YAML manifests")
	outfile := flag.StringP("output", "o", "-", "Output file to write Terraform config")
	providerAlias := flag.StringP("provider", "p", "", "Provider alias to populate the `provider` attribute")
	stripServerSide := flag.BoolP("strip", "s", false, "Strip out server side fields - use if you are piping from kubectl get")
	version := flag.BoolP("version", "V", false, "Show tool version")
	mapOnly := flag.BoolP("map-only", "M", false, "Output only an HCL map structure")
	flag.Parse()

	if *version {
		fmt.Println(toolVersion)
		os.Exit(0)
	}

	var file *os.File
	if *infile == "-" {
		file = os.Stdin
	} else {
		var err error
		file, err = os.Open(*infile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\r\n", err.Error())
			os.Exit(1)
		}
	}

	hcl, err := ToHCL(file, *providerAlias, *stripServerSide, *mapOnly)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if *outfile == "-" {
		fmt.Print(hcl)
	} else {
		ioutil.WriteFile(*outfile, []byte(hcl), 0644)
	}
}
