# 授权模块说明

## 登录相关接口
- **POST /auth/register**
  - `Content-Type: application/json`
  - 请求体：`{"username": "<用户名>", "password": "<密码>"}`（密码需 >= 6 位）
  - 成功响应：`201 Created`，`{"id": <用户ID>, "username": "<用户名>"}`
  - 常见错误：`409` 用户名已存在、`400` 参数缺失或不符合要求。
- **POST /auth/login**
  - `Content-Type: application/json`
  - 请求体：`{"username": "<用户名>", "password": "<密码>"}`
  - 成功响应：`200 OK`，`{"token": "<JWT access token>", "expire": "<到期时间>"}`
  - 常见错误：`401` 未授权（账号或密码错误）、`400` 参数缺失。
- **POST /auth/refresh**
  - `Authorization: Bearer <旧的 access token>` 或 cookie `jwt=<token>`
  - 成功响应：`200 OK`，`{"token": "<新的 access token>", "expire": "<到期时间>"}`
- **GET /auth/profile**
  - `Authorization: Bearer <access token>`
  - 成功响应：`200 OK`，`{"id": <用户ID>, "username": "<用户名>", "roles": ["role"]}`

## 数据库表设计

### users
| 字段 | 类型 | 约束 | 说明 |
| --- | --- | --- | --- |
| id | uint | PK | 主键 |
| username | varchar(64) | UNIQUE, NOT NULL | 登录名 |
| password_hash | varchar(255) | NOT NULL | bcrypt 哈希后的密码 |
| status | varchar(32) | 默认 `active` | 用户状态 |
| last_login_at | datetime | NULL | 最近登录时间 |
| created_at | datetime | NOT NULL | 创建时间 |
| updated_at | datetime | NOT NULL | 更新时间 |

### roles
| 字段 | 类型 | 约束 | 说明 |
| --- | --- | --- | --- |
| id | uint | PK | 主键 |
| name | varchar(64) | UNIQUE, NOT NULL | 角色名称 |
| code | varchar(64) | UNIQUE, NOT NULL | 角色编码 |
| created_at | datetime | NOT NULL | 创建时间 |
| updated_at | datetime | NOT NULL | 更新时间 |

### user_roles
| 字段 | 类型 | 约束 | 说明 |
| --- | --- | --- | --- |
| id | uint | PK | 主键 |
| user_id | uint | NOT NULL, UNIQUE(`idx_user_role`) | 用户外键 |
| role_id | uint | NOT NULL, UNIQUE(`idx_user_role`) | 角色外键 |
| created_at | datetime | NOT NULL | 分配时间 |

> 以上结构与 `authorization/module.go` 中的 `AutoMigrate` 保持一致，可按业务扩展角色权限或刷新令牌等表。
