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




-- 1) agents：智能体基础 + 外观
CREATE TABLE `agents` (
                          `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键ID',
                          `name`              VARCHAR(100)    NOT NULL COMMENT '智能体名称（默认语言）',
                          `gender`            ENUM('male','female','neutral','other') DEFAULT 'neutral' COMMENT '性别/人称：male/female/neutral/other',
                          `title_address`     VARCHAR(50)     DEFAULT NULL COMMENT '对用户的称呼，如：老板/同学/亲',
                          `persona_desc`      TEXT            DEFAULT NULL COMMENT '人设描述（默认语言）',
                          `opening_line`      TEXT            DEFAULT NULL COMMENT '开场白（默认语言）',
                          `first_turn_hint`   TEXT            DEFAULT NULL COMMENT '首轮对话提示（默认语言）',
                          `live2d_model_id`   VARCHAR(100)    DEFAULT NULL COMMENT '绑定的 Live2D 模型ID',
                          `status`            ENUM('draft','active','paused','archived') DEFAULT 'draft' COMMENT '状态：草稿/启用/暂停/归档',
                          `lang_default`      VARCHAR(10)     NOT NULL DEFAULT 'zh-CN' COMMENT '默认语言代码，如 zh-CN',
                          `tags`              JSON            DEFAULT NULL COMMENT '业务标签JSON数组，如["客服","电商"]',
                          `version`           INT             NOT NULL DEFAULT 1 COMMENT '版本号（便于回滚与审计）',
                          `notes`             TEXT            DEFAULT NULL COMMENT '运营/产品备注',
                          `created_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                          `updated_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                          PRIMARY KEY (`id`),
                          KEY `idx_agents_status` (`status`),
                          KEY `idx_agents_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
COMMENT='智能体主表：基础信息与默认语言文案';

-- 2) agent_chat_config：模型与对话配置
CREATE TABLE `agent_chat_config` (
                                     `agent_id`          BIGINT UNSIGNED NOT NULL COMMENT '关联 agents.id（1:1）',
                                     `model_provider`    VARCHAR(50)     NOT NULL COMMENT '模型提供方，如 openai/anthropic/locals',
                                     `model_name`        VARCHAR(100)    NOT NULL COMMENT '模型名，如 gpt-4o-mini',
                                     `model_params`      JSON            DEFAULT NULL COMMENT '模型参数JSON，例：{"temperature":0.3,"max_tokens":1024}',
                                     `system_prompt`     MEDIUMTEXT      DEFAULT NULL COMMENT '系统提示词（与人设分存，便于A/B与回滚）',
                                     `style_guide`       JSON            DEFAULT NULL COMMENT '风格指南JSON，如口吻、长度、禁用表情等',
                                     `response_format`   ENUM('text','markdown','json') DEFAULT 'text' COMMENT '输出格式约定',
                                     `citation_required` TINYINT(1)      NOT NULL DEFAULT 0 COMMENT '是否强制引用检索来源',
                                     `function_calling`  TINYINT(1)      NOT NULL DEFAULT 0 COMMENT '是否启用函数调用/工具调用',
                                     `rag_params`        JSON            DEFAULT NULL COMMENT 'RAG 参数JSON，例：{"top_k":4,"min_score":0.6}',
                                     `created_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                                     `updated_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                                     PRIMARY KEY (`agent_id`),
                                     CONSTRAINT `fk_cfg_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
COMMENT='智能体对话与模型配置';

-- 3) agent_locales：多语言文案覆盖
CREATE TABLE `agent_locales` (
                                 `agent_id`        BIGINT UNSIGNED NOT NULL COMMENT '关联 agents.id',
                                 `lang_code`       VARCHAR(10)     NOT NULL COMMENT '语言代码，如 zh-CN/en-US',
                                 `name`            VARCHAR(100)    DEFAULT NULL COMMENT '名称（多语言覆盖）',
                                 `title_address`   VARCHAR(50)     DEFAULT NULL COMMENT '对用户称呼（多语言覆盖）',
                                 `persona_desc`    TEXT            DEFAULT NULL COMMENT '人设描述（多语言覆盖）',
                                 `opening_line`    TEXT            DEFAULT NULL COMMENT '开场白（多语言覆盖）',
                                 `first_turn_hint` TEXT            DEFAULT NULL COMMENT '首轮提示（多语言覆盖）',
                                 `extras`          JSON            DEFAULT NULL COMMENT '其它可本地化文案JSON',
                                 `updated_at`      DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                                 PRIMARY KEY (`agent_id`, `lang_code`),
                                 CONSTRAINT `fk_loc_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
COMMENT='智能体多语言文案覆盖表（缺省回退至 agents 默认语言字段）';

-- 1) conversations：会话实例
CREATE TABLE `conversations` (
                                 `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '会话ID',
                                 `agent_id`         BIGINT UNSIGNED NOT NULL COMMENT '关联智能体 agents.id',
                                 `user_id`          BIGINT UNSIGNED NOT NULL COMMENT '关联用户 app_users.id',
                                 `title`            VARCHAR(200)    DEFAULT NULL COMMENT '会话标题（可由首轮自动生成）',
                                 `summary`          TEXT            DEFAULT NULL COMMENT '会话摘要（便于回放与检索）',
                                 `channel`          ENUM('web','mobile','wechat','feishu','slack','api') DEFAULT 'web' COMMENT '接入渠道',
                                 `lang`             VARCHAR(10)     DEFAULT NULL COMMENT '会话语言（默认继承用户/智能体）',
                                 `status`           ENUM('active','archived','ended') DEFAULT 'active' COMMENT '会话状态',
                                 `retention_days`   INT             DEFAULT 30 COMMENT '会话保留期（天），便于合规清理',
                                 `token_input_sum`  INT             DEFAULT 0 COMMENT '累计输入token',
                                 `token_output_sum` INT             DEFAULT 0 COMMENT '累计输出token',
                                 `started_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '开始时间',
                                 `last_msg_at`      DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '最近消息时间',
                                 `created_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                                 `updated_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                                 PRIMARY KEY (`id`),
                                 KEY `idx_conv_user_agent` (`user_id`,`agent_id`,`status`),
                                 KEY `idx_conv_last_msg` (`last_msg_at`),
                                 CONSTRAINT `fk_conv_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON DELETE CASCADE,
                                 CONSTRAINT `fk_conv_user`  FOREIGN KEY (`user_id`)  REFERENCES `app_users`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会话实例（用户×智能体×渠道）';

-- 2) messages：消息表
CREATE TABLE `messages` (
                            `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '消息ID',
                            `conversation_id`   BIGINT UNSIGNED NOT NULL COMMENT '所属会话ID',
                            `seq`               INT             NOT NULL COMMENT '会话内顺序号（从1递增，便于排序）',
                            `role`              ENUM('system','user','assistant','tool') NOT NULL COMMENT '角色',
                            `format`            ENUM('text','markdown','json') DEFAULT 'text' COMMENT '内容格式',
                            `content`           MEDIUMTEXT      DEFAULT NULL COMMENT '消息正文（可能为JSON字符串）',
                            `parent_msg_id`     BIGINT UNSIGNED DEFAULT NULL COMMENT '父消息（分叉/追问时可用）',
                            `latency_ms`        INT             DEFAULT NULL COMMENT 'LLM耗时/工具耗时',
                            `token_input`       INT             DEFAULT NULL COMMENT '该轮输入token',
                            `token_output`      INT             DEFAULT NULL COMMENT '该轮输出token',
                            `err_code`          VARCHAR(50)     DEFAULT NULL COMMENT '错误码（若失败）',
                            `err_msg`           VARCHAR(255)    DEFAULT NULL COMMENT '错误信息（若失败）',
                            `created_at`        DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                            PRIMARY KEY (`id`),
                            UNIQUE KEY `uk_msg_conv_seq` (`conversation_id`,`seq`),
                            KEY `idx_msg_conv_created` (`conversation_id`,`created_at`),
                            KEY `idx_msg_parent` (`parent_msg_id`),
                            CONSTRAINT `fk_msg_conv` FOREIGN KEY (`conversation_id`) REFERENCES `conversations`(`id`) ON DELETE CASCADE,
                            CONSTRAINT `fk_msg_parent` FOREIGN KEY (`parent_msg_id`) REFERENCES `messages`(`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会话消息记录（含多轮与分叉）';