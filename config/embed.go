package config

import _ "embed"

//go:embed init_db.sql
var DefaultInitDB string

//go:embed mycnf/default.cnf
var MycnfDefault string

//go:embed mycnf/mariadb10.cnf
var MycnfMariaDB10 string

//go:embed mycnf/mysql57.cnf
var MycnfMySQL57 string

//go:embed mycnf/mysql80.cnf
var MycnfMySQL80 string

//go:embed mycnf/mysql8026.cnf
var MycnfMySQL8026 string

//go:embed mycnf/mysql84.cnf
var MycnfMySQL84 string

//go:embed mycnf/mysql90.cnf
var MycnfMySQL90 string
