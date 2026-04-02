package lua

import (
	"os"
	rt "github.com/arnodel/golua/runtime"
	"touchgocore/util"
	"touchgocore/vars"
)

// info Lua 内置函数：输出 info 日志
func info(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	msg, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}
	vars.Info("%s", msg)
	return c.Next(), nil
}

// debug Lua 内置函数：输出 debug 日志
func debug(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	msg, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}
	vars.Debug("%s", msg)
	return c.Next(), nil
}

// error1 Lua 内置函数：输出 error 日志
func error1(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	msg, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}
	vars.Error("%s", msg)
	return c.Next(), nil
}

// dofile Lua 内置函数：加载并执行 Lua 文件
func dofile(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	filePath, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}

	// 读取并编译文件
	source, err := os.ReadFile(filePath)
	if err != nil {
		return pushErrorResult(t, c, err)
	}

	chunk, err := t.CompileAndLoadLuaChunk(filePath, source, rt.TableValue(t.GlobalEnv()))
	if err != nil {
		return pushErrorResult(t, c, err)
	}

	// 执行脚本
	_, err = rt.Call1(t.MainThread(), rt.FunctionValue(chunk))
	if err != nil {
		return pushErrorResult(t, c, err)
	}

	return pushSuccessResult(t, c)
}

// pushErrorResult 推送错误结果到 Lua 栈
func pushErrorResult(t *rt.Thread, c *rt.GoCont, err error) (rt.Cont, error) {
	next := c.Next()
	t.Push1(next, rt.BoolValue(false))
	t.Push1(next, rt.StringValue(err.Error()))
	return next, nil
}

// pushSuccessResult 推送成功结果到 Lua 栈
func pushSuccessResult(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	next := c.Next()
	t.Push1(next, rt.BoolValue(true))
	t.Push1(next, rt.StringValue("ok"))
	return next, nil
}

// getpathluafile Lua 内置函数：获取指定路径下所有 Lua 文件
func getpathluafile(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	path, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}

	fileList := util.GetPathFile(path, []string{LuaFileExt})

	// 创建 Lua table
	tbl := rt.NewTable()
	for i, file := range fileList {
		t.SetTable(tbl, rt.IntValue(int64(i+1)), rt.StringValue(file))
	}

	next := c.Next()
	t.Push1(next, rt.TableValue(tbl))
	return next, nil
}
