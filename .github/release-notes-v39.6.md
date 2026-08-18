# Local Content Share v39.6

- 修复 Android App 新建文字时普通表单提交返回 500 的问题。
- `/submit` 现在同时兼容 `multipart/form-data` 和 `application/x-www-form-urlencoded`。
- 普通表单下不再访问空的 multipart 文件字段，避免服务端空指针错误。
- 保存成功后 App 可正常重新同步并显示新建文本。
