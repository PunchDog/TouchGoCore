package config

/*
数据库配置结构体
*/
type DBConfig struct {
	Host         string `json:"db_host"`           //连接地址
	Username     string `json:"db_username"`       //用户名
	Password     string `json:"db_password"`       //用户密码
	Name         string `json:"db_name"`           //数据库名
	MaxIdleConns int    `json:"db_max_idle_conns"` //连接池最大空闲连接数
	MaxOpenConns int    `json:"db_max_open_conns"` //连接池最大连接数
}

type RedisConfig struct {
	Host     string `json:"redis_host"`     //连接地址
	Password string `json:"redis_password"` //用户密码
	Db       int    `json:"redis_db"`       //库编号
}
