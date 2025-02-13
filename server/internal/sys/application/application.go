package application

import (
	"mayfly-go/internal/sys/infrastructure/persistence"
	"mayfly-go/pkg/ioc"
)

func init() {
	persistence.Init()

	ioc.Register(new(accountAppImpl), ioc.WithComponentName("AccountApp"))
	ioc.Register(new(roleAppImpl), ioc.WithComponentName("RoleApp"))
	ioc.Register(new(configAppImpl), ioc.WithComponentName("ConfigApp"))
	ioc.Register(new(resourceAppImpl), ioc.WithComponentName("ResourceApp"))
	ioc.Register(new(syslogAppImpl), ioc.WithComponentName("SyslogApp"))
}

func GetAccountApp() Account {
	return ioc.Get[Account]("AccountApp")
}

func GetConfigApp() Config {
	return ioc.Get[Config]("ConfigApp")
}

func GetResourceApp() Resource {
	return ioc.Get[Resource]("ResourceApp")
}

func GetRoleApp() Role {
	return ioc.Get[Role]("RoleApp")
}

func GetSyslogApp() Syslog {
	return ioc.Get[Syslog]("SyslogApp")
}
