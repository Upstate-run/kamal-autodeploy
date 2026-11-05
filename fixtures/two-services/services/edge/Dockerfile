# The fixture's "edge" service: a tiny static site standing in for Phoenix Edge.
FROM nginx:1.27-alpine
COPY public/ /usr/share/nginx/html/
COPY nginx.conf /etc/nginx/conf.d/default.conf
