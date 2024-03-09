// Code generated by "go generate"; DO NOT EDIT.

package paths

import "fmt"

func Modules(organization string) string {
	return fmt.Sprintf("/app/organizations/%s/modules", organization)
}

func CreateModule(organization string) string {
	return fmt.Sprintf("/app/organizations/%s/modules/create", organization)
}

func NewModule(organization string) string {
	return fmt.Sprintf("/app/organizations/%s/modules/new", organization)
}

func Module(module string) string {
	return fmt.Sprintf("/app/modules/%s", module)
}

func EditModule(module string) string {
	return fmt.Sprintf("/app/modules/%s/edit", module)
}

func UpdateModule(module string) string {
	return fmt.Sprintf("/app/modules/%s/update", module)
}

func DeleteModule(module string) string {
	return fmt.Sprintf("/app/modules/%s/delete", module)
}

func RefreshModule(module string) string {
	return fmt.Sprintf("/app/modules/%s/refresh", module)
}
