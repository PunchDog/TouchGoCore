--相对路径
--注意基准：dofile / getpathluafile 会先把路径限制在「脚本根目录」内再解析，
--这里的 .. 是相对进程工作目录，不是相对本文件
dirpath = "../../"

--加载lua文件
--dofile 返回的是 Go 侧约定的 (bool, string)，不是数值错误码：
--写成 iret < 0 会先抛「attempt to compare boolean with number」，
--把真正的「路径越界 / 文件不存在」盖掉
local doFile = function(path)
    local ok, msg = dofile(path)
    if not ok then
        error("加载lua文件" .. path .. "出错:" .. tostring(msg))
    else
        info("load ok: " .. path)
    end
end

--查询路径下文件
local initAllLua = function(path)
    local files, msg = getpathluafile(path)
    if type(files) ~= "table" then
        -- 失败时返回的同样是 (false, msg)，直接 pairs() 会崩
        error("查询lua文件列表失败: " .. tostring(path) .. " -> " .. tostring(msg))
    end
    for _, file in pairs(files) do
        doFile(file)
    end
end

--初始化所有函数
initAllLua(dirpath .. "lua/npc/")
