ALTER TABLE oauth_login_states ADD COLUMN link_user_id INTEGER REFERENCES users(id);
