-- 认证相关表

CREATE TABLE IF NOT EXISTS `users` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
  `username` VARCHAR(64) NOT NULL COMMENT '唯一登录名',
  `password_hash` VARCHAR(255) NOT NULL COMMENT '密码的 bcrypt 哈希',
  `status` VARCHAR(32) NOT NULL DEFAULT 'active' COMMENT '账户状态',
  `last_login_at` DATETIME DEFAULT NULL COMMENT '上次成功登录时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  CONSTRAINT `pk_users` PRIMARY KEY (`id`),
  CONSTRAINT `uk_users_username` UNIQUE (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='应用账户表';

CREATE TABLE IF NOT EXISTS `roles` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
  `name` VARCHAR(64) NOT NULL COMMENT '角色显示名',
  `code` VARCHAR(64) NOT NULL COMMENT '稳定的机器标识符',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  CONSTRAINT `pk_roles` PRIMARY KEY (`id`),
  CONSTRAINT `uk_roles_name` UNIQUE (`name`),
  CONSTRAINT `uk_roles_code` UNIQUE (`code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='角色目录';

CREATE TABLE IF NOT EXISTS `user_roles` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '关联 users.id',
  `role_id` BIGINT UNSIGNED NOT NULL COMMENT '关联 roles.id',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT `pk_user_roles` PRIMARY KEY (`id`),
  CONSTRAINT `uk_user_roles_user_role` UNIQUE (`user_id`, `role_id`),
  CONSTRAINT `fk_user_roles_user` FOREIGN KEY (`user_id`) REFERENCES `users`(`id`) ON UPDATE CASCADE ON DELETE CASCADE,
  CONSTRAINT `fk_user_roles_role` FOREIGN KEY (`role_id`) REFERENCES `roles`(`id`) ON UPDATE CASCADE ON DELETE CASCADE,
  INDEX `idx_user_roles_user` (`user_id`),
  INDEX `idx_user_roles_role` (`role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='用户与角色的多对多关联';

-- 代理与会话相关表

CREATE TABLE IF NOT EXISTS `agents` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
  `name` VARCHAR(100) NOT NULL COMMENT '显示名称',
  `gender` ENUM('male','female','neutral','other') NOT NULL DEFAULT 'neutral' COMMENT '形象呈现',
  `title_address` VARCHAR(50) DEFAULT NULL COMMENT '面向用户的称谓',
  `persona_desc` TEXT DEFAULT NULL COMMENT '角色长描述',
  `opening_line` TEXT DEFAULT NULL COMMENT '首轮对话默认问候',
  `first_turn_hint` TEXT DEFAULT NULL COMMENT '引导首条回复的提示',
  `live2d_model_id` VARCHAR(100) DEFAULT NULL COMMENT '关联的 Live2D 模型标识',
  `status` ENUM('draft','active','paused','archived') NOT NULL DEFAULT 'draft' COMMENT '生命周期状态',
  `lang_default` VARCHAR(10) NOT NULL DEFAULT 'zh-CN' COMMENT '默认语言代码',
  `tags` JSON DEFAULT NULL COMMENT '业务标签 JSON 数组',
  `version` INT NOT NULL DEFAULT 1 COMMENT '乐观锁版本号',
  `notes` TEXT DEFAULT NULL COMMENT '内部备注',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_agents_status` (`status`),
  KEY `idx_agents_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='代理（角色）主数据';

CREATE TABLE IF NOT EXISTS `agent_chat_config` (
  `agent_id` BIGINT UNSIGNED NOT NULL COMMENT '关联 agents.id',
  `model_provider` VARCHAR(50) NOT NULL COMMENT '例如 openai、anthropic、azure',
  `model_name` VARCHAR(100) NOT NULL COMMENT '模型标识',
  `model_params` JSON DEFAULT NULL COMMENT 'LLM 参数覆盖',
  `system_prompt` MEDIUMTEXT DEFAULT NULL COMMENT '会话历史前追加的系统提示',
  `style_guide` JSON DEFAULT NULL COMMENT '额外风格约束',
  `response_format` ENUM('text','markdown','json') NOT NULL DEFAULT 'text' COMMENT '期望响应格式',
  `citation_required` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '为 1 时强制引用',
  `function_calling` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '启用工具调用',
  `rag_params` JSON DEFAULT NULL COMMENT '检索配置',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`agent_id`),
  CONSTRAINT `fk_agent_chat_config_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON UPDATE CASCADE ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='每个代理的聊天运行配置';

CREATE TABLE IF NOT EXISTS `agent_locales` (
  `agent_id` BIGINT UNSIGNED NOT NULL COMMENT '关联 agents.id',
  `lang_code` VARCHAR(10) NOT NULL COMMENT 'IETF 语言代码',
  `name` VARCHAR(100) DEFAULT NULL COMMENT '本地化显示名称',
  `title_address` VARCHAR(50) DEFAULT NULL COMMENT '本地化称谓',
  `persona_desc` TEXT DEFAULT NULL COMMENT '本地化角色描述',
  `opening_line` TEXT DEFAULT NULL COMMENT '本地化开场白',
  `first_turn_hint` TEXT DEFAULT NULL COMMENT '本地化首轮提示',
  `extras` JSON DEFAULT NULL COMMENT '其他本地化字段',
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`agent_id`, `lang_code`),
  CONSTRAINT `fk_agent_locales_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON UPDATE CASCADE ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='代理文案的本地化覆盖';

CREATE TABLE IF NOT EXISTS `conversations` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
  `agent_id` BIGINT UNSIGNED NOT NULL COMMENT '关联 agents.id',
  `user_id` BIGINT UNSIGNED NOT NULL COMMENT '关联 users.id',
  `title` VARCHAR(200) DEFAULT NULL COMMENT '可选会话标题',
  `summary` TEXT DEFAULT NULL COMMENT '自动生成的摘要',
  `channel` ENUM('web','mobile','wechat','feishu','slack','api') NOT NULL DEFAULT 'web' COMMENT '来源渠道',
  `lang` VARCHAR(10) DEFAULT NULL COMMENT '会话语言',
  `status` ENUM('active','archived','ended') NOT NULL DEFAULT 'active' COMMENT '状态',
  `retention_days` INT DEFAULT 30 COMMENT '保留天数',
  `token_input_sum` INT DEFAULT 0 COMMENT '输入 Token 累计',
  `token_output_sum` INT DEFAULT 0 COMMENT '输出 Token 累计',
  `started_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '会话开始时间',
  `last_msg_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '最近消息时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_conversations_user_agent` (`user_id`, `agent_id`, `status`),
  KEY `idx_conversations_last_msg` (`last_msg_at`),
  CONSTRAINT `fk_conversations_agent` FOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON UPDATE CASCADE ON DELETE CASCADE,
  CONSTRAINT `fk_conversations_user` FOREIGN KEY (`user_id`) REFERENCES `users`(`id`) ON UPDATE CASCADE ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='用户与代理的会话记录';

CREATE TABLE IF NOT EXISTS `messages` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键',
  `conversation_id` BIGINT UNSIGNED NOT NULL COMMENT '关联 conversations.id',
  `seq` INT UNSIGNED NOT NULL COMMENT '会话内从 1 开始的顺序号',
  `role` ENUM('system','user','assistant','tool') NOT NULL COMMENT '说话角色',
  `format` ENUM('text','markdown','json') NOT NULL DEFAULT 'text' COMMENT '负载格式',
  `content` MEDIUMTEXT DEFAULT NULL COMMENT '消息正文或序列化负载',
  `parent_msg_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '可选的父消息（用于线程）',
  `latency_ms` INT DEFAULT NULL COMMENT '助手生成延迟（毫秒）',
  `token_input` INT DEFAULT NULL COMMENT '发送给 LLM 的 Token',
  `token_output` INT DEFAULT NULL COMMENT 'LLM 返回的 Token',
  `err_code` VARCHAR(50) DEFAULT NULL COMMENT '生成失败时的错误码',
  `err_msg` VARCHAR(255) DEFAULT NULL COMMENT '人类可读的错误摘要',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_messages_conv_seq` (`conversation_id`, `seq`),
  KEY `idx_messages_conv_created` (`conversation_id`, `created_at`),
  KEY `idx_messages_parent` (`parent_msg_id`),
  CONSTRAINT `fk_messages_conversation` FOREIGN KEY (`conversation_id`) REFERENCES `conversations`(`id`) ON UPDATE CASCADE ON DELETE CASCADE,
  CONSTRAINT `fk_messages_parent` FOREIGN KEY (`parent_msg_id`) REFERENCES `messages`(`id`) ON UPDATE CASCADE ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='会话消息历史';
