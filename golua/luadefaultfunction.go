package lua

import (
	"os"
	rt "github.com/arnodel/golua/runtime"
	"touchgocore/util"
	"touchgocore/vars"
)

// Lua 内置函数：info
func info(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	err := c.CheckNArgs(1)
	if err != nil {
		return nil, err
	}

	msg, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}

	vars.Info("%s", msg)

	// 无返回值
	return c.Next(), nil
}

// Lua 内置函数：debug
func debug(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	err := c.CheckNArgs(1)
	if err != nil {
		return nil, err
	}

	msg, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}

	vars.Debug("%s", msg)

	// 无返回值
	return c.Next(), nil
}

// Lua 内置函数：error
func error1(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	err := c.CheckNArgs(1)
	if err != nil {
		return nil, err
	}

	msg, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}

	vars.Error("%s", msg)

	// 无返回值
	return c.Next(), nil
}

// Lua 内置函数：dofile
func dofile(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	err := c.CheckNArgs(1)
	if err != nil {
		return nil, err
	}

	filePath, err := c.StringArg(0)
	if err != nil {
		return nil, err
	}

	// 读取并编译文件
	source, err := os.ReadFile(filePath)
	if err != nil {
		// 返回 (false, error message)
		next := c.Next()
		t.Push1(next, rt.BoolValue(false))
		t.Push1(next, rt.StringValue(err.Error()))
		return next, nil
	}

	chunk, err := t.CompileAndLoadLuaChunk(filePath, source, rt.TableValue(t.GlobalEnv()))
	if err != nil {
		// 返回 (false, error message)
		next := c.Next()
		t.Push1(next, rt.BoolValue(false))
		t.Push1(next, rt.StringValue(err.Error()))
		return next, nil
	}

	// 执行脚本
	_, err = rt.Call1(t.MainThread(), rt.FunctionValue(chunk))
	if err != nil {
		// 返回 (false, error message)
		next := c.Next()
		t.Push1(next, rt.BoolValue(false))
		t.Push1(next, rt.StringValue(err.Error()))
		return next, nil
	}

	// 返回 (true, "ok")
	next := c.Next()
	t.Push1(next, rt.BoolValue(true))
	t.Push1(next, rt.StringValue("ok"))
	return next, nil
}

// GetLuaFiles 获取指定路径下所有 Lua 文件
func getpathluafile(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	err := c.CheckNArgs(1)
	if err != nil {
		return nil, err
	}

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

	// 返回 table
	next := c.Next()
	t.Push1(next, rt.TableValue(tbl))
	return next, nil
}
