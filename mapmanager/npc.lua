-- ============================================================================
-- NPC配置文件 - TouchGoCore
-- ============================================================================
-- 对话类型常量
local DialogType = {
    NORMAL   = "normal",   -- 普通说话
    DIALOG   = "dialog",  -- 对话框(带选项)
    SHOP     = "shop",    -- 商店
    PRIVATE  = "private", -- 密语/关键字触发
    SEND     = "send",    -- 传送
    CLOSE    = "close",   -- 关闭对话框
    TASK     = "task",    -- 任务
}

-- 普通说话随机模式
local NormalRandMode = {
    OFF      = "off",     -- 不随机，使用固定text
    ONCE     = "once",    -- 创建时随机选一条（后续固定）
    EVERY    = "every",   -- 每次触发对话都重新随机
}

-- 商店选择模式
local ShopMode = {
    DIRECT   = "direct",  -- 直接打开单个商店
    RAND     = "rand",    -- 随机选择一个商店
    FIXED    = "fixed",   -- 显示多个商店按钮供选择
}

-- 限购刷新类型
local RefreshType = {
    DAY      = "day",     -- 每日刷新
    WEEK     = "week",    -- 每周刷新
    NEVER    = "0",       -- 不刷新(不限购)
}

-- ============================================================================
-- NPC创建函数
-- @param config NPC配置表
-- ============================================================================
local createNpc = function(config)
    -- 参数校验
    if not config then
        error("[NPC] 配置表为空，无法创建NPC")
        return
    end
    if not config.id or config.id <= 0 then
        error("[NPC] 配置无效: id 必须大于0")
        return
    end

    -- 创建NPC实例
    local npc = Npc:new(config.id)
    if not npc then
        error("[NPC] 创建NPC实例失败: id=" .. tostring(config.id))
        return
    end

    -- 设置基础属性
    npc:SetName(config.name or ("NPC_" .. config.id))
    npc:SetShape(config.shape or "default")
    npc:SetDirection(config.direction or 1)
    npc:SetAutoMove(config.auto_move or false)
    npc:SetMapId(config.map_id or 0)

    -- 设置移动路径点
    -- 格式: {{x1,y1}, {x2,y2}, ...} 或 {{x1,y1,z1}, {x2,y2,z2}, ...}
    if config.points and #config.points > 0 then
        for _, point in ipairs(config.points) do
            local x = tonumber(point[1]) or 0
            local y = tonumber(point[2]) or 0
            -- 修复: 只传递2个参数 (原代码错误地传了3个)
            npc:AddMapPoint(x, y)
        end
    end

    -- 设置商店数据
    if config.shops then
        for shopId, items in pairs(config.shops) do
            if type(items) == "table" then
                for _, item in ipairs(items) do
                    npc:AddShop(
                        shopId,
                        item[1] or 0,   -- item_id
                        item[2] or 0,   -- cost_type
                        item[3] or 0,   -- cost
                        item[4] or 0,   -- max_buy_cnt
                        item[5] or "0"  -- refresh_type
                    )
                end
            end
        end
    end

    -- 设置对话数据
    if config.dialogs then
        for dialogId, dialogData in pairs(config.dialogs) do
            local text = dialogData.text or ""
            local dialogType = dialogData.type or DialogType.NORMAL

            -- 普通说话类型: 支持随机文本
            -- 当 dialogData.texts 为数组时，根据 rand_mode 决定随机策略
            -- rand_mode = "off"/nil  → 不随机，仍使用 text 字段
            -- rand_mode = "once"     → 创建时随机选一条
            -- rand_mode = "every"    → 每次对话都随机（通过param1传递texts数组）
            if dialogType == DialogType.NORMAL and type(dialogData.texts) == "table" and #dialogData.texts > 0 then
                local randMode = dialogData.rand_mode or NormalRandMode.ONCE
                if randMode == NormalRandMode.ONCE then
                    -- 创建时随机选一条，之后固定
                    text = dialogData.texts[math.random(#dialogData.texts)]
                elseif randMode == NormalRandMode.EVERY then
                    -- 每次触发时随机: text设为第一条（占位），texts数组存入param1
                    text = dialogData.texts[1]
                    if not dialogData.param1 then
                        dialogData.param1 = dialogData.texts
                    end
                end
                -- rand_mode == "off" 时不做任何处理
            end

            npc:AddDialog(
                dialogId,
                text,
                dialogType,
                dialogData.param1,
                dialogData.param2,
                dialogData.param3
            )
        end
    end

    return npc
end

-- ============================================================================
-- 示例: 创建测试NPC
-- ============================================================================
local testNpcConfig = {
    -- 基础信息
    id          = 1,
    name        = "测试NPC",
    shape       = "test001",
    direction   = 1,
    auto_move   = true,
    map_id      = 1001,

    -- 移动路径点 (x, y 坐标对)
    points = {
        {100, 100},
        {200, 200},
        {100, 100},
        {200, 200},
    },

    -- 对话列表 (dialog_id => 对话配置)
    dialogs = {
        -- 普通说话: 随机文本（创建时随机选一条）
        [0] = {
            texts = {
                "今天天气真好",
                "欢迎来到我们的村庄",
                "最近有什么新鲜事吗？",
                "你看起来很精神啊！",
            },
            rand_mode = NormalRandMode.ONCE,  -- 创建时随机，后续固定
            type = DialogType.NORMAL,
        },

        -- 普通说话: 每次触发都随机
        [6] = {
            texts = {
                "哼，别来烦我！",
                "我今天心情不好...",
                "走开走开！",
                "别挡路！",
            },
            rand_mode = NormalRandMode.EVERY,  -- 每次对话都重新随机
            type = DialogType.NORMAL,
        },

        -- 对话框: 带选项的交互
        [1] = {
            text = "你好!有什么需要帮忙的吗?",
            type = DialogType.DIALOG,
            -- param1: 对话选项列表
            -- 格式: {{前置对话ID, 前置对话执行次数, 目标对话ID}, ...}
            param1 = {
                {0, 0, 10},   -- 前置条件满足则显示选项
                {0, 0, 11},
            },
        },

        -- 商店NPC: 打开商店界面
        [2] = {
            text = "欢迎光临商店!",
            type = DialogType.SHOP,
            param1 = ShopMode.RAND,    -- 选择模式: direct/rand/fixed
            param2 = {1, 2},            -- 商店ID列表
        },

        -- 关键字触发: 密语类型
        [3] = {
            text = "密语内容",
            type = DialogType.PRIVATE,
            param1 = "密语内容",         -- 关键字
            param2 = 2,                -- 触发后跳转的对话ID
        },

        -- 传送NPC: 传送玩家到指定位置
        [4] = {
            text = "要传送到主城吗?",
            type = DialogType.SEND,
            param1 = 0,                -- 0=传送NPC到目标位置, 1=传送玩家
            param2 = {1001, 100, 1000}, -- {地图ID, X坐标, Y坐标}
        },

        -- 任务NPC
        [5] = {
            text = "这里有个任务要交给你",
            type = DialogType.TASK,
            param1 = 1001,             -- 任务ID
        },

        -- 选项A: 打开商店
        [10] = {
            text = "打开商店",
            type = DialogType.DIALOG,
            param1 = {{0, 0, 2}},      -- 选择后跳转到对话ID=2
        },

        -- 选项B: 接受任务
        [11] = {
            text = "接受任务",
            type = DialogType.DIALOG,
            param1 = {{0, 0, 5}},
        },

        -- 关闭对话框
        [99] = {
            text = "再见",
            type = DialogType.CLOSE,
        },
    },

    -- 商店配置 (shop_id => 商品列表)
    shops = {
        -- 商店1: 武器店
        [1] = {
            {2001, 1000, 10000, 99, RefreshType.DAY},   -- {物品ID, 货币类型, 价格, 限购数量, 刷新类型}
            {2002, 1000, 15000, 5,  RefreshType.WEEK},
        },
        -- 商店2: 防具店
        [2] = {
            {3001, 1000, 20000, 10, RefreshType.DAY},
        },
    },
}

-- ============================================================================
-- 执行创建
-- ============================================================================
local npc = createNpc(testNpcConfig)
if npc then
    print("[NPC] NPC创建成功: id=" .. testNpcConfig.id .. ", name=" .. testNpcConfig.name)
else
    print("[NPC] NPC创建失败")
end

-- 清理Lua端临时数据
Npc:destory()

-- ============================================================================
-- 使用示例: 创建多个NPC
-- ============================================================================
--[[
local function createMultipleNpcs()
    -- 城镇NPC
    createNpc({
        id = 1001,
        name = "卫兵",
        shape = "guard_001",
        direction = 2,
        map_id = 1001,
        dialogs = {
            [0] = { texts = {"站住!请出示通行证", "谁在那边?"}, rand_mode = NormalRandMode.ONCE, type = DialogType.NORMAL },
            [1] = { text = "关闭", type = DialogType.CLOSE },
        },
    })

    -- 商人NPC
    createNpc({
        id = 1002,
        name = "杂货商",
        shape = "merchant_001",
        direction = 1,
        map_id = 1001,
        points = {{300, 400}, {350, 400}, {350, 450}, {300, 450}},
        dialogs = {
            [0] = { texts = {"收购!收购!最新鲜的货物!", "便宜卖了!便宜卖了!"}, rand_mode = NormalRandMode.ONCE, type = DialogType.NORMAL },
            [1] = { text = "打开商店", type = DialogType.SHOP, param1 = ShopMode.DIRECT, param2 = {10} },
            [99] = { text = "下次再来", type = DialogType.CLOSE },
        },
        shops = {
            [10] = {
                {1001, 1000, 100, 999, RefreshType.NEVER},
            },
        },
    })

    -- 传送NPC
    createNpc({
        id = 2001,
        name = "传送师",
        shape = "teleporter_001",
        direction = 1,
        map_id = 1001,
        dialogs = {
            [0] = { texts = {"想去哪里旅行?", "远方在召唤你!"}, rand_mode = NormalRandMode.EVERY, type = DialogType.NORMAL },
            [1] = { text = "去主城", type = DialogType.SEND, param1 = 1, param2 = {1, 500, 500} },
            [2] = { text = "去野外", type = DialogType.SEND, param1 = 1, param2 = {2, 100, 100} },
            [99] = { text = "算了", type = DialogType.CLOSE },
        },
    })
end

createMultipleNpcs()
Npc.destory()
--]]
