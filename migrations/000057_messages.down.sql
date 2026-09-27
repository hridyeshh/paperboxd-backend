DELETE FROM reports WHERE content_type = 'message';
DROP TABLE IF EXISTS message_attachments;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversation_members;
DROP TABLE IF EXISTS conversations;
