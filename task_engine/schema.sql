-- taskengine 表结构（MySQL 8.0，utf8mb4）
--
-- 任务定义 + 每周期一行的进度。进度口径由 progress_type 决定：
--   INVITE = 用 ProgressParmInvite 覆盖进度
--   ADD    = 用 ProgressParmAdd 累加进度
-- 达成阈值是 target_count（float64）；status 只有 active/completed/claimed。
--
-- 建表后由宿主按自己的业务写入 task_def；本包不提供后台 CRUD 服务。

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `task_def` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `title` varchar(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `desc` varchar(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `trigger` varchar(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
  `cycle` varchar(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
  `grant_mode` varchar(8) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
  `progress_type` varchar(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'ADD',
  `reward_json` text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci COMMENT '奖励配置（JSON），引擎不解析',
  `cycle_limit` int NOT NULL DEFAULT '1',
  `target_count` double NOT NULL DEFAULT '1' COMMENT '达成阈值；<=0 按 1',
  `target_count_type` varchar(1) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '=' COMMENT '对比条件：> / = / <',
  `enabled` tinyint NOT NULL DEFAULT '1',
  `start_at` datetime DEFAULT NULL,
  `end_at` datetime DEFAULT NULL,
  `pre_task_id` bigint NOT NULL DEFAULT '0' COMMENT '前置任务配置 ID，0=无',
  `group` varchar(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '' COMMENT '任务分组标签，空=未分组',
  `clear_on_cycle` tinyint NOT NULL DEFAULT '1' COMMENT '1=周期切换清空进度，0=保留（引擎不消费）',
  `next_day_claim` tinyint NOT NULL DEFAULT '0' COMMENT '1=第二天才能领奖（引擎不消费）',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `ix_task_def_trigger_enabled` (`trigger`,`enabled`),
  KEY `idx_task_def_group` (`group`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='任务定义';

CREATE TABLE IF NOT EXISTS `task_progress` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `member_id` bigint NOT NULL,
  `task_id` bigint NOT NULL,
  `cycle_key` varchar(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
  `progress` bigint NOT NULL DEFAULT '0',
  `status` varchar(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active',
  `snapshot_json` text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '最近一次 ProgressParm 快照',
  `created_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `ix_task_progress_cycle` (`member_id`,`task_id`,`cycle_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='任务进度（每周期一行）';
