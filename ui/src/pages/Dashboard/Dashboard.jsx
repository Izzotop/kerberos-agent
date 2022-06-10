import React from 'react';
import { Breadcrumb } from '@kerberos-io/ui';
// import { Link } from 'react-router-dom';
import styles from './Dashboard.scss';

// eslint-disable-next-line react/prefer-stateless-function
class Dashboard extends React.Component {
  render() {
    return (
      <div className={styles.dashboard}>
        <Breadcrumb
          title="Dashboard"
          level1="Overview of your video surveilance"
          level1Link=""
        >
          {/* <Link to="/deployments">
            <Button
              label="Add Kerberos Agent"
              icon="plus-circle"
              type="default"
            />
    </Link> */}
        </Breadcrumb>
      </div>
    );
  }
}
export default Dashboard;
