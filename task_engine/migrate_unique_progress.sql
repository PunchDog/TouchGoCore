-- task_progress 唯一键迁移（MySQL 8.0）
--
-- 目的：把 (member_id, task_id, cycle_key) 从普通索引升级为唯一键，
-- 让统一缓存层的 UpsertBatch（ON DUPLICATE KEY UPDATE）落库生效。
--
-- 执行顺序：
--   1. 去重：同键只保留 id 最大的一行（即最新状态），旧行进 _bak 备份表；
--   2. 加唯一键；
--   3. 可选：删除被唯一键覆盖的旧普通索引。
--
-- 备份表可保留一段时间用于核对，确认无误后再 DROP。

SET NAMES utf8mb4;

-- 1. 备份重复行（保留每个键 id 最大的一行）。
CREATE TABLE IF NOT EXISTS `task_progress_dup_bak` LIKE `task_progress`;

INSERT INTO `task_progress_dup_bak`
SELECT p.*
FROM `task_progress` p
WHERE EXISTS (
  SELECT 1
  FROM `task_progress` q
  WHERE q.`member_id` = p.`member_id`
    AND q.`task_id` = p.`task_id`
    AND q.`cycle_key` = p.`cycle_key`
    AND q.`id` > p.`id`
);

DELETE p
FROM `task_progress` p
JOIN `task_progress` q
  ON p.`member_id` = q.`member_id`
 AND p.`task_id` = q.`task_id`
 AND p.`cycle_key` = q.`cycle_key`
 AND p.`id` < q.`id`;

-- 2. 加唯一键。
ALTER TABLE `task_progress`
  ADD UNIQUE KEY `uk_task_progress_cycle` (`member_id`,`task_id`,`cycle_key`);

-- 3. 旧普通索引已被唯一键覆盖，可按需删除。
-- ALTER TABLE `task_progress` DROP INDEX `ix_task_progress_cycle`;