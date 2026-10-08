package config

import "gopkg.in/yaml.v3"

func BootstrapYAML() []byte {
	c := Default()
	c.SetupRequired = true
	c.Server.Management.Listen = ":3378"
	b, err := yaml.Marshal(c)
	if err != nil {
		panic(err)
	}
	return b
}
