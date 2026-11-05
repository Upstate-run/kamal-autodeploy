# The fixture's "rails" service: a tiny static site standing in for the Rails app.
FROM nginx:1.27-alpine
COPY public/ /usr/share/nginx/html/
COPY app/nginx.conf /etc/nginx/conf.d/default.conf
