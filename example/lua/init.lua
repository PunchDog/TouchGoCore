--相对路径
dirpath = "../../"

--加载lua文件
local doFile = function(path)
    local iret, msg = dofile(path)
    if iret < 0 then
        error("加载lua文件" .. path .. "出错:" .. msg)
    else
        info("load ok: " .. path)
    end
end

--查询路径下文件
local initAllLua = function(path)
    local files = getpathluafile(path)
    for index, file in pairs(files) do
        doFile(file)
    end
end

--初始化所有函数
initAllLua(dirpath .. "lua/npc/")
