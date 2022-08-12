--创建NPC
local createnpc = function()
    --NPCID
    local id = 1
    --NPC名字
    local name = "test npc"
    --形象
    local Shape = "test001"
    --朝向,8个
    local Direction = 1
    --是否是自动行动NPC
    local auto = true
    --地图坐标,两个坐标一组{mapx,mapy},可以有多个,如果是行动类NPC，同地图自动移动
    local point = {
        {100, 100},
        {200, 200},
        {100, 100},
        {200, 200}
    }
    --对话,默认执行最前面一条说话或者对话框,如果带有密语类型，优先判断密语类型
    local dialog = {
        [0] = {"今天天气真好", "normarl"}, --说话类型
        [1] = {"你好!", "dialog", {{1, 10, 4}, {0, 0, 10}}}, --对话框型,{{0, 0, 4}, {0, 0, 10}}为对话框选项，第一个0前置对话ID，第二个0是前置对话ID执行次数,第三个数是目前显示的对话选项
        [2] = {"打开商店", "shop", "rand", {1, 2}}, --商店类型,{1,2}表示两个shop对应的商店，如果只配置一个，则直接打开商品界面,如果是rand，就在{1,2}随机一个商店列表，如果是fixed，就是两个商店按钮
        [3] = {"密语", "privatekey", "密语内容", 2}, --密语类型,内容一样就打开对应的对话ID
        [4] = {"传送", "send", 0, {1001, 100, 1000}}, --传送类型,{1001, 100, 1000}为地图坐标,0表示传送NPC,1表示传送说话的玩家
        [10] = {"关闭", "close"}
    }
    --商店设置,可以有多个商店页面
    local shop = {
        [1] = {
            {2001, 1000, 10000, 99, "day"} --商品列表，{物品ID，物品价格类型，物品价格，物品限购个数，限购个数刷新时间:day每日;week每周;0不限购}
        },
        [2] = {}
    }

    --------------------------------------------------------开始写内存数据--------------------------------------------------
    local npc = Npc:new(id)--创建NPC类
    npc:SetName(name)--设置名字
    npc:SetShape(Shape)--设置形象
    npc:SetDirection(Direction)--设置朝向
    npc:SetAutoMove(auto)--设置自动行走
    for index, tbl in pairs(point) do--设置地图坐标
        npc:AddMapPoint(tbl[1], tbl[2], tbl[3])
    end
    for shopid, itemlist in pairs(shop) do--设置商店
        for key, item in pairs(itemlist) do
            npc:AddShop(shopid, item[1], item[2], item[3], item[4], item[5])
        end
    end
    Npc:destory()--删除NPC类容器
end

createnpc()
