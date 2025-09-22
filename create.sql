-- 1) users
CREATE TABLE IF NOT EXISTS `users` (
                                       `id` INT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
                                       `username` VARCHAR(64) NOT NULL COMMENT '登录名',
    `password_hash` VARCHAR(255) NOT NULL COMMENT 'bcrypt 哈希后的密码',
    `status` VARCHAR(32) NOT NULL DEFAULT 'active' COMMENT '用户状态',
    `last_login_at` DATETIME NULL DEFAULT NULL COMMENT '最近登录时间',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    CONSTRAINT `pk_users` PRIMARY KEY (`id`),
    CONSTRAINT `uk_users_username` UNIQUE (`username`)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 2) roles
CREATE TABLE IF NOT EXISTS `roles` (
                                       `id` INT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
                                       `name` VARCHAR(64) NOT NULL COMMENT '角色名称',
    `code` VARCHAR(64) NOT NULL COMMENT '角色编码',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    CONSTRAINT `pk_roles` PRIMARY KEY (`id`),
    CONSTRAINT `uk_roles_name` UNIQUE (`name`),
    CONSTRAINT `uk_roles_code` UNIQUE (`code`)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 3) user_roles（用户-角色多对多映射）
CREATE TABLE IF NOT EXISTS `user_roles` (
                                            `id` INT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
                                            `user_id` INT UNSIGNED NOT NULL COMMENT '用户外键',
                                            `role_id` INT UNSIGNED NOT NULL COMMENT '角色外键',
                                            `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '分配时间',
                                            CONSTRAINT `pk_user_roles` PRIMARY KEY (`id`),
    -- 组合唯一：同一用户-角色只允许一条
    CONSTRAINT `uk_user_roles_user_role` UNIQUE (`user_id`, `role_id`),
    -- 外键（按需可改为 RESTRICT/SET NULL 等策略）
    CONSTRAINT `fk_user_roles_user` FOREIGN KEY (`user_id`)
    REFERENCES `users`(`id`)
    ON UPDATE CASCADE
    ON DELETE CASCADE,
    CONSTRAINT `fk_user_roles_role` FOREIGN KEY (`role_id`)
    REFERENCES `roles`(`id`)
    ON UPDATE CASCADE
    ON DELETE CASCADE,
    -- 常用查询的辅助索引（非必须，但可提升联查性能）
    INDEX `idx_user_roles_user` (`user_id`),
    INDEX `idx_user_roles_role` (`role_id`)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;