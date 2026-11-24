-- 连接配置存入数据库（REQ-005）：模型与 Gitea 连接，全局一套，按类别各一行
-- content 为 jsonb，密钥明文保存（能读取本表的数据库账号即可获得密钥，接入方需收紧本表与备份的访问）
-- gitea: {"base_url","token","webhook_secret","auto_register_webhook","webhook_url","reject_redirects"}
-- model: {"main":{"provider","base_url","api_key","model"},"light":{...}}，provider 取 openai | deepseek
-- version 每次保存 +1（0 表示从未保存），各实例按它判断是否需要刷新
CREATE TABLE lingo_settings (
    category   text        PRIMARY KEY CHECK (category IN ('model', 'gitea')),
    content    jsonb       NOT NULL DEFAULT '{}',
    version    bigint      NOT NULL DEFAULT 0,
    updated_by text        NOT NULL DEFAULT '',
    updated_at timestamptz
);
INSERT INTO lingo_settings (category) VALUES ('model'), ('gitea');
