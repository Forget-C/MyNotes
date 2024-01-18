
### 创建错误页面
`kubectl apply -f error-page.yaml -n ingress-nginx`
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: error-page
  namespace: ingress-nginx
data:
  404.html: |-
    <!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">
    <html xmlns="http://www.w3.org/1999/xhtml">
    <head>
    <meta charset="UTF-8" http-equiv="Content-Type" content="text/html; charset=utf-8" />
    <title>404-对不起！您访问的页面不存在</title>
    <style type="text/css">
    .head404{ width:580px; height:234px; margin:50px auto 0 auto; background:url(https://www.daixiaorui.com/Public/images/head404.png) no-repeat; }
    .txtbg404{ width:499px; height:169px; margin:10px auto 0 auto; background:url(https://www.daixiaorui.com/Public/images/txtbg404.png) no-repeat;}
    .txtbg404 .txtbox{ width:390px; position:relative; top:30px; left:60px;color:#eee; font-size:13px;}
    .txtbg404 .txtbox p {margin:5px 0; line-height:18px;}
    .txtbg404 .txtbox .paddingbox { padding-top:15px;}
    .txtbg404 .txtbox p a { color:#eee; text-decoration:none;}
    .txtbg404 .txtbox p a:hover { color:#FC9D1D; text-decoration:underline;}
    </style>
    </head>
    <body bgcolor="#494949">
    <div class="head404"></div>
    </body>
    </html>
```
可以将yaml中的html代码替换掉。 
### 创建backend
`kubectl apply -f backend.yaml -n ingress-nginx`
```yaml
---
apiVersion: v1
kind: Service
metadata:
  name: nginx-errors
  labels:
    app.kubernetes.io/name: nginx-errors
    app.kubernetes.io/part-of: ingress-nginx
spec:
  selector:
    app.kubernetes.io/name: nginx-errors
    app.kubernetes.io/part-of: ingress-nginx
  ports:
  - port: 80
    targetPort: 8080
    name: http
  - port: 443
    targetPort: 8080
    name: https
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-errors
  labels:
    app.kubernetes.io/name: nginx-errors
    app.kubernetes.io/part-of: ingress-nginx
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: nginx-errors
      app.kubernetes.io/part-of: ingress-nginx
  template:
    metadata:
      labels:
        app.kubernetes.io/name: nginx-errors
        app.kubernetes.io/part-of: ingress-nginx
    spec:
      containers:
      - name: nginx-error-server
        image: quay.io/kubernetes-ingress-controller/custom-error-pages-amd64:0.3
        ports:
        - containerPort: 8080
        volumeMounts:
        - name: error-page
          mountPath: /www/404.html
          subPath: 404.html
      volumes:
      - name: error-page
        configMap:
          name: error-page
        # Setting the environment variable DEBUG we can see the headers sent
        #         # by the ingress controller to the backend in the client response.
        #                 # env:
        #                         # - name: DEBUG
        #                                 #   value: "true"
```

### 修改ingress nginx配置
`kubectl edit cm nginx-configuration -n ingress-nginx` 
添加  `custom-http-errors: 404,503` , 意思是 404和503错误会定向的刚才定义的错误页面。
```yaml
apiVersion: v1
data:
  custom-http-errors: 404,503
  fastcgi_connect_timeout: 600s
  fastcgi_read_timeout: 600s
  fastcgi_send_timeout: 600s
  proxy-body-size: 2048m
  proxy-protocol: "True"
  proxy_connect_timeout: 600s
  proxy_intercept_errors: "True"
  proxy_read_timeout: 600s
  proxy_send_timeout: 600s
  real-ip-header: X-Forwarded-For
  use-forwarded-headers: "true"
kind: ConfigMap
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","kind":"ConfigMap","metadata":{"annotations":{},"labels":{"app.kubernetes.io/name":"ingress-nginx","app.kubernetes.io/part-of":"ingress-nginx"},"name":"nginx-configuration","namespace":"ingress-nginx"}}
  creationTimestamp: "2019-12-23T13:06:06Z"
  labels:
    app.kubernetes.io/name: ingress-nginx
    app.kubernetes.io/part-of: ingress-nginx
  name: nginx-configuration
  namespace: ingress-nginx
  resourceVersion: "70153039"
  selfLink: /api/v1/namespaces/ingress-nginx/configmaps/nginx-configuration
  uid: 079c4b59-295b-4752-8911-a327e549fc6f
```
### 修改ingress ngixn args
编辑ingress部署文件 `kubectl edit deploy nginx-ingress-controller -n ingress-nginx`
在启动的args添加 `--default-backend-service=ingress-nginx/nginx-errors`
### 验证
执行 `kubectl get pod -n ingress-nginx` 查看pod是否已经重启过， 如果没有自动重启，需要杀掉pod。
打开一个不存在的链接， 查看是否显示的是定义的错误页面。
部分浏览器不支持页面显示，如谷歌浏览器会显示“无法显示此网站”