function doFile(path)
    local iret,msg = dofile(path)
    if iret < 0 then
        error("加载lua文件"..path.."出错:"..msg)
    else
        info("load ok: "..path)
    end
end

--初始化所有函数
-- doFile("../../lua/npc/init.lua")
local function initalllua()
    local str = debug.getinfo(2,"S").source.sub(2)
    return str:match("(.*/)")
end

print(initalllua())