# TouchGoCore v0.01
开发方便使用全球服框架
touchgocore:框架基本信息与组件
ezample:实例

当前设计以BusID作为模块组，同一组服务器内部分成exec（主服务器）,dll（功能服务器），模拟动态服务器组模式
目前功能：
1、rpc自动映射服务器网内各个组件，目前支持转发：exec->exec,exec->dll,dll->exec，不同exec之间必须是不同的busid，同组exec和dll对应的busid一样
2、日志功能